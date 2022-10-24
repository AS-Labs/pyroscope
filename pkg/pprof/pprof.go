package pprof

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/gzip"
	"github.com/pkg/errors"
	"github.com/samber/lo"

	profilev1 "github.com/grafana/phlare/pkg/gen/google/v1"
	"github.com/grafana/phlare/pkg/slices"
)

var (
	gzipReaderPool = sync.Pool{
		New: func() any {
			return &gzipReader{
				reader: bytes.NewReader(nil),
			}
		},
	}
	gzipWriterPool = sync.Pool{
		New: func() any {
			return gzip.NewWriter(io.Discard)
		},
	}
	bufPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(nil)
		},
	}
)

type gzipReader struct {
	gzip   *gzip.Reader
	reader *bytes.Reader
}

// open gzip, create reader if required
func (r *gzipReader) gzipOpen() error {
	var err error
	if r.gzip == nil {
		r.gzip, err = gzip.NewReader(r.reader)
	} else {
		err = r.gzip.Reset(r.reader)
	}
	return err
}

func (r *gzipReader) openBytes(input []byte) (io.Reader, error) {
	r.reader.Reset(input)

	// handle if data is not gzipped at all
	if err := r.gzipOpen(); err == gzip.ErrHeader {
		r.reader.Reset(input)
		return r.reader, nil
	} else if err != nil {
		return nil, errors.Wrap(err, "gzip reset")
	}

	return r.gzip, nil
}

func fromUncompressedReader(r io.Reader) (*Profile, error) {
	buf := bufPool.Get().(*bytes.Buffer)

	_, err := io.Copy(buf, r)
	if err != nil {
		return nil, errors.Wrap(err, "copy to buffer")
	}

	p := profilev1.ProfileFromVTPool()
	if err := p.UnmarshalVT(buf.Bytes()); err != nil {
		return nil, err
	}

	return &Profile{Profile: p, buf: buf}, nil
}

// Read RawProfile from bytes
func RawFromBytes(input []byte) (*Profile, error) {
	gzipReader := gzipReaderPool.Get().(*gzipReader)
	defer gzipReaderPool.Put(gzipReader)

	r, err := gzipReader.openBytes(input)
	if err != nil {
		return nil, err
	}

	return fromUncompressedReader(r)
}

// Read Profile from Bytes
func FromBytes(input []byte) (*profilev1.Profile, error) {
	p, err := RawFromBytes(input)
	if err != nil {
		return nil, err
	}

	p.buf.Reset()
	bufPool.Put(p.buf)

	return p.Profile, nil
}

func OpenFile(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return RawFromBytes(data)
}

type Profile struct {
	*profilev1.Profile
	// raw []byte
	buf *bytes.Buffer

	hasher StacktracesHasher
}

func (p *Profile) Close() {
	p.Profile.ReturnToVTPool()
	p.buf.Reset()
	bufPool.Put(p.buf)
}

func (p *Profile) SizeBytes() int {
	return p.buf.Len()
}

// WriteTo writes the profile to the given writer.
func (p *Profile) WriteTo(w io.Writer) (int64, error) {
	// reuse the data buffer if possible
	p.buf.Reset()
	p.buf.Grow(p.SizeVT())
	data := p.buf.Bytes()
	n, err := p.MarshalToVT(data)
	if err != nil {
		return 0, err
	}
	data = data[:n]

	gzipWriter := gzipWriterPool.Get().(*gzip.Writer)
	gzipWriter.Reset(w)
	defer func() {
		// reset gzip writer and return to pool
		gzipWriter.Reset(io.Discard)
		gzipWriterPool.Put(gzipWriter)
	}()

	written, err := gzipWriter.Write(data)
	if err != nil {
		return 0, errors.Wrap(err, "gzip write")
	}
	if err := gzipWriter.Close(); err != nil {
		return 0, errors.Wrap(err, "gzip close")
	}

	// reset buffer
	p.buf.Reset()

	return int64(written), nil
}

type sortedSample struct {
	samples []*profilev1.Sample
	hashes  []uint64
}

func (s *sortedSample) Len() int {
	return len(s.samples)
}

func (s *sortedSample) Less(i, j int) bool {
	return s.hashes[i] < s.hashes[j]
}

func (s *sortedSample) Swap(i, j int) {
	s.samples[i], s.samples[j] = s.samples[j], s.samples[i]
	s.hashes[i], s.hashes[j] = s.hashes[j], s.hashes[i]
}

// Normalize normalizes the profile by:
//   - Removing all duplicate samples (summing their values).
//   - Removing redundant profile labels (byte => unique of an allocation site)
//     todo: We should reassess if this was a good choice because by merging duplicate stacktrace samples
//     we cannot recompute the allocation per site ("bytes") profile label.
//   - Removing empty samples.
//   - Then remove unused references.
func (p *Profile) Normalize() {
	// first we sort the samples location ids.
	hashes := p.hasher.Hashes(p.Sample)

	ss := &sortedSample{samples: p.Sample, hashes: hashes}
	sort.Sort(ss)
	p.Sample = ss.samples
	hashes = ss.hashes

	// Remove samples.
	var removedSamples []*profilev1.Sample

	p.Sample = slices.RemoveInPlace(p.Sample, func(s *profilev1.Sample, i int) bool {
		// if the next sample has the same hashes, we can remove this sample but add the value to the next sample.
		if i < len(p.Sample)-1 && hashes[i] == hashes[i+1] {
			// todo handle hashes collisions
			for j := 0; j < len(s.Value); j++ {
				p.Sample[i+1].Value[j] += s.Value[j]
			}
			removedSamples = append(removedSamples, s)
			return true
		}
		for j := 0; j < len(s.Value); j++ {
			if s.Value[j] != 0 {
				// we found a non-zero value, so we can keep this sample, but remove redundant labels.
				s.Label = slices.RemoveInPlace(s.Label, func(l *profilev1.Label, _ int) bool {
					// remove labels block "bytes" as it's redundant.
					if l.Num != 0 && l.Key != 0 &&
						p.StringTable[l.Key] == "bytes" {
						return true
					}
					return false
				})
				return false
			}
		}
		// all values are 0, remove the sample.
		removedSamples = append(removedSamples, s)
		return true
	})

	// Remove references to removed samples.
	p.clearSampleReferences(removedSamples)
}

func (p *Profile) clearSampleReferences(samples []*profilev1.Sample) {
	if len(samples) == 0 {
		return
	}
	// remove all data not used anymore.
	removedLocationIds := map[uint64]struct{}{}

	for _, s := range samples {
		for _, l := range s.LocationId {
			removedLocationIds[l] = struct{}{}
		}
	}

	// figure which removed Locations IDs are not used.
	for _, s := range p.Sample {
		for _, l := range s.LocationId {
			delete(removedLocationIds, l)
		}
	}
	if len(removedLocationIds) == 0 {
		return
	}
	removedFunctionIds := map[uint64]struct{}{}
	// remove the locations that are not used anymore.
	p.Location = slices.RemoveInPlace(p.Location, func(loc *profilev1.Location, _ int) bool {
		if _, ok := removedLocationIds[loc.Id]; ok {
			for _, l := range loc.Line {
				removedFunctionIds[l.FunctionId] = struct{}{}
			}
			return true
		}
		return false
	})

	if len(removedFunctionIds) == 0 {
		return
	}
	// figure which removed Function IDs are not used.
	for _, l := range p.Location {
		for _, f := range l.Line {
			// 	// that ID is used in another location, remove it.
			delete(removedFunctionIds, f.FunctionId)
		}
	}
	removedNamesMap := map[int64]struct{}{}
	// remove the functions that are not used anymore.
	p.Function = slices.RemoveInPlace(p.Function, func(fn *profilev1.Function, _ int) bool {
		if _, ok := removedFunctionIds[fn.Id]; ok {
			removedNamesMap[fn.Name] = struct{}{}
			removedNamesMap[fn.SystemName] = struct{}{}
			removedNamesMap[fn.Filename] = struct{}{}
			return true
		}
		return false
	})

	if len(removedNamesMap) == 0 {
		return
	}
	// remove names that are still used.
	p.visitAllNameReferences(func(idx *int64) {
		delete(removedNamesMap, *idx)
	})
	if len(removedNamesMap) == 0 {
		return
	}

	// remove the names that are not used anymore.
	p.StringTable = lo.Reject(p.StringTable, func(_ string, i int) bool {
		_, ok := removedNamesMap[int64(i)]
		return ok
	})
	removedNames := lo.Keys(removedNamesMap)
	// Sort to remove in order.
	sort.Slice(removedNames, func(i, j int) bool { return removedNames[i] < removedNames[j] })
	// Now shift all indices [0,1,2,3,4,5,6]
	// if we removed [1,2,5] then we need to shift [3,4] to [1,2] and [6] to [3]
	// Basically we need to shift all indices that are greater than the removed index by the amount of removed indices.
	p.visitAllNameReferences(func(idx *int64) {
		var shift int64
		for i := 0; i < len(removedNames); i++ {
			if *idx > removedNames[i] {
				shift++
				continue
			}
			break
		}
		*idx -= shift
	})
}

func (p *Profile) visitAllNameReferences(fn func(*int64)) {
	fn(&p.DropFrames)
	fn(&p.KeepFrames)
	fn(&p.PeriodType.Type)
	fn(&p.PeriodType.Unit)
	for _, st := range p.SampleType {
		fn(&st.Type)
		fn(&st.Unit)
	}
	for _, m := range p.Mapping {
		fn(&m.Filename)
		fn(&m.BuildId)
	}
	for _, s := range p.Sample {
		for _, l := range s.Label {
			fn(&l.Key)
			fn(&l.Num)
			fn(&l.NumUnit)
		}
	}
	for _, f := range p.Function {
		fn(&f.Name)
		fn(&f.SystemName)
		fn(&f.Filename)
	}
	for i := 0; i < len(p.Comment); i++ {
		fn(&p.Comment[i])
	}
}

type StacktracesHasher struct {
	hash *xxhash.Digest
	b    [8]byte
}

// todo we might want to reuse the results to avoid allocations
func (h StacktracesHasher) Hashes(samples []*profilev1.Sample) []uint64 {
	if h.hash == nil {
		h.hash = xxhash.New()
	} else {
		h.hash.Reset()
	}

	hashes := make([]uint64, len(samples))
	for i, sample := range samples {
		for _, locID := range sample.LocationId {
			binary.LittleEndian.PutUint64(h.b[:], locID)
			if _, err := h.hash.Write(h.b[:]); err != nil {
				panic("unable to write hash")
			}
		}
		hashes[i] = h.hash.Sum64()
		h.hash.Reset()
	}

	return hashes
}
