import { defineStore } from 'pinia'
import { ref } from 'vue'
import { teamsService, type Team, type TeamMember } from '@/services/api'

export interface CreateTeamData {
  name: string
  description?: string
  assignment_strategy?: 'round_robin' | 'load_balanced' | 'manual'
  per_agent_timeout_secs?: number
}

export interface UpdateTeamData {
  name?: string
  description?: string
  assignment_strategy?: 'round_robin' | 'load_balanced' | 'manual'
  per_agent_timeout_secs?: number
  is_active?: boolean
}

export interface FetchTeamsParams {
  search?: string
  page?: number
  limit?: number
}

export interface FetchTeamsResponse {
  teams: Team[]
  total: number
  page: number
  limit: number
}

export const useTeamsStore = defineStore('teams', () => {
  const teams = ref<Team[]>([])
  const loading = ref(false)
  const error = ref<string | null>(null)
  let identityGeneration = 0
  let fetchGeneration = 0
  let fetchController: AbortController | null = null

  function resetForIdentityChange() {
    identityGeneration++
    fetchGeneration++
    fetchController?.abort()
    fetchController = null
    teams.value = []
    loading.value = false
    error.value = null
  }

  async function fetchTeams(params?: FetchTeamsParams): Promise<FetchTeamsResponse> {
    const identity = identityGeneration
    const requestGeneration = ++fetchGeneration
    fetchController?.abort()
    const controller = new AbortController()
    fetchController = controller
    loading.value = true
    error.value = null
    try {
      const response = await teamsService.list(params, controller.signal)
      const data = (response.data as any).data || response.data
      const fetchedTeams = data.teams || []
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        teams.value = fetchedTeams
      }
      return {
        teams: fetchedTeams,
        total: data.total ?? fetchedTeams.length,
        page: data.page ?? 1,
        limit: data.limit ?? 50
      }
    } catch (err: any) {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        error.value = err.response?.data?.message || 'Failed to fetch teams'
      }
      throw err
    } finally {
      if (
        identity === identityGeneration &&
        requestGeneration === fetchGeneration
      ) {
        loading.value = false
        if (fetchController === controller) fetchController = null
      }
    }
  }

  async function createTeam(data: CreateTeamData): Promise<Team> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await teamsService.create(data)
      const newTeam = (response.data as any).data?.team || response.data?.team
      if (identity === identityGeneration) teams.value.unshift(newTeam)
      return newTeam
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to create team'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function updateTeam(id: string, data: UpdateTeamData): Promise<Team> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      const response = await teamsService.update(id, data)
      const updatedTeam = (response.data as any).data?.team || response.data?.team
      if (identity === identityGeneration) {
        const index = teams.value.findIndex(t => t.id === id)
        if (index !== -1) {
          teams.value[index] = updatedTeam
        }
      }
      return updatedTeam
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to update team'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function deleteTeam(id: string): Promise<void> {
    const identity = identityGeneration
    loading.value = true
    error.value = null
    try {
      await teamsService.delete(id)
      if (identity === identityGeneration) {
        teams.value = teams.value.filter(t => t.id !== id)
      }
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to delete team'
      }
      throw err
    } finally {
      if (identity === identityGeneration) loading.value = false
    }
  }

  async function fetchTeamMembers(teamId: string): Promise<TeamMember[]> {
    const identity = identityGeneration
    try {
      const response = await teamsService.listMembers(teamId)
      if (identity !== identityGeneration) return []
      return (response.data as any).data?.members || response.data?.members || []
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to fetch team members'
      }
      throw err
    }
  }

  async function addTeamMember(teamId: string, userId: string, role: 'manager' | 'agent' = 'agent'): Promise<TeamMember> {
    const identity = identityGeneration
    try {
      const response = await teamsService.addMember(teamId, { user_id: userId, role })
      // Update member count
      if (identity === identityGeneration) {
        const team = teams.value.find(t => t.id === teamId)
        if (team) {
          team.member_count = (team.member_count || 0) + 1
        }
      }
      return (response.data as any).data?.member || response.data?.member
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to add team member'
      }
      throw err
    }
  }

  async function removeTeamMember(teamId: string, userId: string): Promise<void> {
    const identity = identityGeneration
    try {
      await teamsService.removeMember(teamId, userId)
      // Update member count
      if (identity === identityGeneration) {
        const team = teams.value.find(t => t.id === teamId)
        if (team && team.member_count > 0) {
          team.member_count = team.member_count - 1
        }
      }
    } catch (err: any) {
      if (identity === identityGeneration) {
        error.value = err.response?.data?.message || 'Failed to remove team member'
      }
      throw err
    }
  }

  return {
    teams,
    loading,
    error,
    fetchTeams,
    createTeam,
    updateTeam,
    deleteTeam,
    fetchTeamMembers,
    addTeamMember,
    removeTeamMember,
    resetForIdentityChange,
  }
})
