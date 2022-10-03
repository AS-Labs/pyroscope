import { DataQueryRequest, DataQueryResponse, DataSourceInstanceSettings } from '@grafana/data';
import { DataSourceWithBackend } from '@grafana/runtime';
import { FireDataSourceOptions, Query, ProfileTypeMessage, SeriesMessage } from './types';
import { Observable, of } from 'rxjs';

export class FireDataSource extends DataSourceWithBackend<Query, FireDataSourceOptions> {
  constructor(instanceSettings: DataSourceInstanceSettings<FireDataSourceOptions>) {
    super(instanceSettings);
  }

  query(request: DataQueryRequest<Query>): Observable<DataQueryResponse> {
    const validTargets = request.targets.filter((t) => t.profileTypeId);
    if (!validTargets.length) {
      return of({ data: [] });
    }
    return super.query(request);
  }

  async getProfileTypes(): Promise<ProfileTypeMessage[]> {
    return await super.getResource('profileTypes');
  }

  async getSeries(): Promise<SeriesMessage> {
    // For now, we send empty matcher to get all the series
    return await super.getResource('series', { matchers: ['{}'] });
  }

  async getLabelNames(): Promise<string[]> {
    return await super.getResource('labelNames');
  }
}
