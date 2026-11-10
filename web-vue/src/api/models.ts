import type { MediaCapability } from '@/lib/mediaCapabilities'
import apiClient from './client'

export interface OpenAIModel {
  id: string
  name?: string
  object?: string
  created?: number
  owned_by?: string
  root?: string
  parent?: string | null
  capability?: string
  media_capabilities?: MediaCapability
}

export interface ModelListResponse {
  object?: string
  data: OpenAIModel[]
}

export interface ModelCatalogResponse {
  media_capabilities?: Record<string, MediaCapability>
  object?: 'model_catalog' | string
  chat_models: string[]
  image_models: string[]
  image_edit_models?: string[]
  video_models?: string[]
  all_models?: string[]
  models?: OpenAIModel[]
  model_aliases?: Record<string, string>
  grok_web_modes?: Record<string, { mode_id: string; verified_accounts: number; available_accounts: number }>
  source?: {
    chat?: string
    image?: string
  }
  openai_models_endpoint?: string
}

export const modelsApi = {
  catalog: () => apiClient.get<never, ModelCatalogResponse>('/api/model-catalog'),
  list: () => apiClient.get<never, ModelListResponse>('/v1/models'),
}
