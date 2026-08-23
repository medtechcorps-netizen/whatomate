import { defineStore } from 'pinia'
import { ref } from 'vue'
import { tagsService, type Tag } from '@/services/api'

export interface CreateTagData {
  name: string
  color?: string
}

export interface UpdateTagData {
  name?: string
  color?: string
}

export interface FetchTagsParams {
  search?: string
  page?: number
  limit?: number
}

export interface FetchTagsResponse {
  tags: Tag[]
  total: number
  page: number
  limit: number
}

export const useTagsStore = defineStore('tags', () => {
  const tags = ref<Tag[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let identityGeneration = 0
  let fetchGeneration = 0

  function resetForIdentityChange() {
    identityGeneration++
    fetchGeneration++
    tags.value = []
    loading.value = false
    error.value = null
  }

  async function fetchTags(params?: FetchTagsParams): Promise<FetchTagsResponse> {
    const identity = identityGeneration
    const requestGeneration = ++fetchGeneration
    loading.value = true
    error.value = null
    try {
      const response = await tagsService.list(params)
      const data = (response.data as any).data || response.data
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        tags.value = data.tags || []
      }
      return {
        tags: data.tags || [],
        total: data.total ?? tags.value.length,
        page: data.page ?? 1,
        limit: data.limit ?? 50
      }
    } catch (err: any) {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch tags'
      }
      throw err
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        loading.value = false
      }
    }
  }

  async function createTag(data: CreateTagData): Promise<Tag> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await tagsService.create(data)
      const newTag = (response.data as any).data || response.data
      if (identity === identityGeneration) {
        tags.value.push(newTag)
        // Sort by name
        tags.value.sort((a, b) => a.name.localeCompare(b.name))
      }
      return newTag
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to create tag'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function updateTag(name: string, data: UpdateTagData): Promise<Tag> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await tagsService.update(name, data)
      const updatedTag = (response.data as any).data || response.data
      if (identity === identityGeneration) {
        const index = tags.value.findIndex(t => t.name === name)
        if (index !== -1) {
          tags.value[index] = updatedTag
        }
        // Re-sort if name changed
        if (data.name && data.name !== name) {
          tags.value.sort((a, b) => a.name.localeCompare(b.name))
        }
      }
      return updatedTag
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to update tag'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function deleteTag(name: string): Promise<void> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      await tagsService.delete(name)
      if (identity === identityGeneration) {
        tags.value = tags.value.filter(t => t.name !== name)
      }
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to delete tag'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  function getTagByName(name: string): Tag | undefined {
    return tags.value.find(t => t.name === name)
  }

  return {
    tags,
    loading,
    error,
    fetchTags,
    createTag,
    updateTag,
    deleteTag,
    getTagByName,
    resetForIdentityChange,
  }
})
