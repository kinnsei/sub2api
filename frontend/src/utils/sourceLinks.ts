import { computed } from 'vue'
import { useAppStore } from '@/stores/app'

/**
 * Builds source/documentation links from the deployment's configured release
 * repository (`update.repository`, surfaced as `source_repository`).
 *
 * Why not hardcode a repository: a fork must not silently send its own users to
 * another project's pages — those pages may carry unrelated branding, referral
 * codes, or documentation that does not describe the running build. When no
 * repository is configured the links stay empty and callers hide them, rather
 * than guessing an address.
 */

const REPOSITORY_PATTERN = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/

export function useSourceLinks() {
  const appStore = useAppStore()

  const repository = computed(() => (appStore.sourceRepository || '').trim())

  const hasRepository = computed(() => REPOSITORY_PATTERN.test(repository.value))

  const repositoryUrl = computed(() =>
    hasRepository.value ? `https://github.com/${repository.value}` : '',
  )

  function githubUrlFor(repo: string): string {
    const trimmed = repo.trim()
    return REPOSITORY_PATTERN.test(trimmed) ? `https://github.com/${trimmed}` : ''
  }

  /** Blob (rendered file) URL for a repo-relative path, e.g. `docs/PAYMENT_CN.md`. */
  function blobUrlFor(repo: string, path: string, ref = 'production'): string {
    const base = githubUrlFor(repo)
    const cleanPath = path.replace(/^\/+/, '')
    return base && cleanPath ? `${base}/blob/${ref}/${cleanPath}` : ''
  }

  const blobUrl = (path: string, ref = 'production') =>
    blobUrlFor(repository.value, path, ref)

  return {
    repository,
    hasRepository,
    repositoryUrl,
    githubUrlFor,
    blobUrlFor,
    blobUrl,
  }
}
