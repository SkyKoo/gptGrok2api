import apiClient from './client'
export interface ModelRoute {
  model: string
  name?: string
  channels: string[]
  priority: string[]
}
export interface ModelRouting { models: ModelRoute[] }
export const modelRoutingApi = {
  get: () => apiClient.get<never, ModelRouting>('/api/model-routing'),
  update: (priorities: Record<string, string[]>) => apiClient.put<unknown, ModelRouting>('/api/model-routing', { priorities }),
}
