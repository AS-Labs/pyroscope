package symdb

import (
	"math"

	googlev1 "github.com/grafana/pyroscope/api/gen/proto/go/google/v1"
	"github.com/grafana/pyroscope/pkg/model"
	schemav1 "github.com/grafana/pyroscope/pkg/phlaredb/schemas/v1"
	"github.com/grafana/pyroscope/pkg/slices"
)

type pprofProtoSymbols struct {
	profile googlev1.Profile
	symbols *Symbols
	samples *schemav1.Samples
	tree    *model.StacktraceTree
	cur     int

	maxNodes  int64
	truncated int

	rewriteTable []uint32
}

func (r *pprofProtoSymbols) init(symbols *Symbols, samples schemav1.Samples) {
	r.symbols = symbols
	r.samples = &samples
	r.maxNodes = 16 << 10
	r.tree = model.NewStacktraceTree(samples.Len() * 4)
}

func (r *pprofProtoSymbols) InsertStacktrace(_ uint32, locations []int32) {
	r.tree.Insert(locations, int64(r.samples.Values[r.cur]))
	r.cur++
}

func (r *pprofProtoSymbols) buildPprof() *googlev1.Profile {
	r.truncated = r.tree.Truncate(r.tree.MinValue(r.maxNodes))
	truncatedRoot := &googlev1.Sample{
		LocationId: []uint64{0},
		Value:      []int64{0},
	}
	// The actual number of samples is not know in advance,
	// if some of the branches were truncated.
	r.profile.Sample = make([]*googlev1.Sample, 0, r.cur)
	tmp := make([]int32, 64)
	// Now we resolve each and every stack trace in the tree:
	// a leaf node indicates the stack trace tip.
	for i, n := range r.tree.Nodes[1:] {
		if n.FirstChild > 0 {
			// Not a leaf.
			continue
		}
		if r.tree.Nodes[n.Parent].Location < 0 {
			// Truncated.
			continue
		}
		tmp = r.tree.Resolve(tmp, int32(i))
		// Skip stack traces truncated to the root.
		if len(tmp) == 1 && tmp[0] < 0 {
			truncatedRoot.Value[0] += n.Value
			continue
		}
		s := &googlev1.Sample{
			LocationId: make([]uint64, len(tmp)),
			Value:      []int64{n.Value},
		}
		for l, v := range tmp {
			if v < 0 {
				// Truncated location reference (-1)
				// needs to be replaced with the stub
				// we created.
				s.LocationId[l] = math.MaxUint64
			} else {
				s.LocationId[l] = uint64(v)
			}
		}
		r.profile.Sample = append(r.profile.Sample, s)
	}
	if truncatedRoot.Value[0] > 0 {
		r.profile.Sample = append(r.profile.Sample, truncatedRoot)
	}
	r.copyLocations()
	r.copyFunctions()
	r.copyMappings()
	r.copyStrings()
	if r.truncated > 0 {
		// We have truncated some branches,
		// therefore need a stub location.
		r.createStub()
	}
	return &r.profile
}

const truncatedNodeName = "other"

func (r *pprofProtoSymbols) createStub() {
	name := int64(len(r.profile.StringTable))
	r.profile.StringTable = append(r.profile.StringTable, truncatedNodeName)
	fn := &googlev1.Function{
		Id:         uint64(len(r.profile.Function) + 1),
		Name:       name,
		SystemName: name,
	}
	r.profile.Function = append(r.profile.Function, fn)
	stubLoc := &googlev1.Location{
		Id:        uint64(len(r.profile.Location) + 1),
		Line:      []*googlev1.Line{{FunctionId: fn.Id}},
		MappingId: 1,
	}
	r.profile.Location = append(r.profile.Location, stubLoc)
	for _, s := range r.profile.Sample {
		for i, loc := range s.LocationId {
			if loc == math.MaxUint64 {
				s.LocationId[i] = stubLoc.Id
			}
		}
	}
}

func (r *pprofProtoSymbols) copyLocations() {
	r.profile.Location = make([]*googlev1.Location, len(r.symbols.Locations))
	for _, n := range r.tree.Nodes[1:] {
		if n.Location >= 0 && r.profile.Location[n.Location] == nil {
			src := r.symbols.Locations[n.Location]
			loc := &googlev1.Location{
				Id:        src.Id,
				MappingId: uint64(src.MappingId),
				Address:   src.Address,
				Line:      make([]*googlev1.Line, len(src.Line)),
				IsFolded:  src.IsFolded,
			}
			for i, line := range src.Line {
				loc.Line[i] = &googlev1.Line{
					FunctionId: uint64(line.FunctionId),
					Line:       int64(line.Line),
				}
			}
			r.profile.Location[n.Location] = loc
		}
	}
	n := len(r.profile.Location)
	r.rewriteTable = slices.GrowLen(r.rewriteTable, n+1)
	var j int
	for i := 0; i < len(r.profile.Location); i++ {
		loc := r.profile.Location[i]
		if loc == nil {
			continue
		}
		oldId := loc.Id
		loc.Id = uint64(j) + 1
		r.rewriteTable[oldId] = uint32(loc.Id)
		r.profile.Location[j] = loc
		j++
	}
	r.profile.Location = r.profile.Location[:j]
	for _, s := range r.profile.Sample {
		for i, loc := range s.LocationId {
			if loc != math.MaxUint64 {
				s.LocationId[i] = uint64(r.rewriteTable[loc])
			}
		}
	}
}

func (r *pprofProtoSymbols) copyFunctions() {
	r.profile.Function = make([]*googlev1.Function, len(r.symbols.Functions))
	for _, loc := range r.profile.Location {
		for _, line := range loc.Line {
			if r.profile.Function[line.FunctionId] == nil {
				src := r.symbols.Functions[line.FunctionId]
				r.profile.Function[line.FunctionId] = &googlev1.Function{
					Id:         src.Id,
					Name:       int64(src.Name),
					SystemName: int64(src.SystemName),
					Filename:   int64(src.Filename),
					StartLine:  int64(src.StartLine),
				}
			}
		}
	}
	n := len(r.profile.Function)
	r.rewriteTable = slices.GrowLen(r.rewriteTable, n+1)
	var j int
	for i := 0; i < len(r.profile.Function); i++ {
		fn := r.profile.Function[i]
		if fn == nil {
			continue
		}
		oldId := fn.Id
		fn.Id = uint64(j) + 1
		r.rewriteTable[oldId] = uint32(fn.Id)
		r.profile.Function[j] = fn
		j++
	}
	r.profile.Function = r.profile.Function[:j]
	for _, loc := range r.profile.Location {
		for _, line := range loc.Line {
			line.FunctionId = uint64(r.rewriteTable[line.FunctionId])
		}
	}
}

func (r *pprofProtoSymbols) copyMappings() {
	r.profile.Mapping = make([]*googlev1.Mapping, len(r.symbols.Mappings))
	for _, loc := range r.profile.Location {
		if r.profile.Mapping[loc.MappingId] == nil {
			src := r.symbols.Mappings[loc.MappingId]
			r.profile.Mapping[loc.MappingId] = &googlev1.Mapping{
				Id:              src.Id,
				MemoryStart:     src.MemoryStart,
				MemoryLimit:     src.MemoryLimit,
				FileOffset:      src.FileOffset,
				Filename:        int64(src.Filename),
				BuildId:         int64(src.BuildId),
				HasFunctions:    src.HasFunctions,
				HasFilenames:    src.HasFilenames,
				HasLineNumbers:  src.HasLineNumbers,
				HasInlineFrames: src.HasInlineFrames,
			}
		}
	}
	n := len(r.profile.Mapping)
	r.rewriteTable = slices.GrowLen(r.rewriteTable, n+1)
	var j int
	for i := 0; i < len(r.profile.Mapping); i++ {
		m := r.profile.Mapping[i]
		if m == nil {
			continue
		}
		oldId := m.Id
		m.Id = uint64(j) + 1
		r.rewriteTable[oldId] = uint32(m.Id)
		r.profile.Mapping[j] = m
		j++
	}
	r.profile.Mapping = r.profile.Mapping[:j]
	for _, loc := range r.profile.Location {
		loc.MappingId = uint64(r.rewriteTable[loc.MappingId])
	}
}

func (r *pprofProtoSymbols) copyStrings() {
	r.profile.StringTable = make([]string, len(r.symbols.Strings), len(r.symbols.Strings)+1)
	r.profile.StringTable[0] = ""
	for _, m := range r.profile.Mapping {
		if m.Filename != 0 && r.profile.StringTable[m.Filename] == "" {
			r.profile.StringTable[m.Filename] = r.symbols.Strings[m.Filename]
		}
		if m.BuildId != 0 && r.profile.StringTable[m.BuildId] == "" {
			r.profile.StringTable[m.BuildId] = r.symbols.Strings[m.BuildId]
		}
	}
	for _, f := range r.profile.Function {
		if f.Name != 0 && r.profile.StringTable[f.Name] == "" {
			r.profile.StringTable[f.Name] = r.symbols.Strings[f.Name]
		}
		if f.Filename != 0 && r.profile.StringTable[f.Filename] == "" {
			r.profile.StringTable[f.Filename] = r.symbols.Strings[f.Filename]
		}
		if f.SystemName != 0 && r.profile.StringTable[f.SystemName] == "" {
			r.profile.StringTable[f.SystemName] = r.symbols.Strings[f.SystemName]
		}
	}
	n := len(r.profile.StringTable)
	r.rewriteTable = slices.GrowLen(r.rewriteTable, n+1)
	var j int
	for i := 0; i < len(r.profile.StringTable); i++ {
		s := r.profile.StringTable[i]
		if s == "" {
			continue
		}
		r.rewriteTable[i] = uint32(j)
		r.profile.StringTable[j] = s
		j++
	}
	r.profile.StringTable = r.profile.StringTable[:j]
	for _, m := range r.profile.Mapping {
		m.Filename = int64(r.rewriteTable[m.Filename])
		m.BuildId = int64(r.rewriteTable[m.BuildId])
	}
	for _, f := range r.profile.Function {
		f.Name = int64(r.rewriteTable[f.Name])
		f.Filename = int64(r.rewriteTable[f.Filename])
		f.SystemName = int64(r.rewriteTable[f.SystemName])
	}
}
