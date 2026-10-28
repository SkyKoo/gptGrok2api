import apiClient from './client'
import { buildVideoForm, type StudioVideoOptions } from '@/lib/studioMedia'

export interface VideoTask {
  id: string
  status: 'queued' | 'in_progress' | 'completed' | 'failed'
  progress: number
  error?: { code?: string; message?: string }
}
export const videosApi = {
  create: (prompt: string, options: StudioVideoOptions, files: File[]) =>
    apiClient.post<unknown, VideoTask>('/v1/videos', buildVideoForm(prompt, options, files)),
  get: (id: string) => apiClient.get<unknown, VideoTask>(`/v1/videos/${encodeURIComponent(id)}`),
  content: (id: string, signal?: AbortSignal) => apiClient.get<unknown, Blob>(`/v1/videos/${encodeURIComponent(id)}/content`, { responseType: 'blob', signal }),
}
