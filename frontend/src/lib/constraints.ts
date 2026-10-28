
export const EMAIL_MAX_LENGTH = 254

export const DISPLAY_NAME_MAX_LENGTH = 100

export const AVATAR_URL_MAX_LENGTH = 2048

/** Avatar upload caps, mirrors of internal/service/avatar.go. */
export const AVATAR_UPLOAD_MAX_BYTES = 5 * 1024 * 1024
export const AVATAR_IMAGE_MAX_DIMENSION = 512
export const AVATAR_ACCEPTED_TYPES = 'image/png,image/jpeg' as const

/** Profile identity caps, mirrors of internal/service/system.go (validateProfileSection). */
export const PROFILE_BIO_MAX_LENGTH = 500

export const PROFILE_JOB_TITLE_MAX_LENGTH = 100

/** Embedding model id cap, a mirror of internal/service/system.go (maxEmbeddingModelChars). */
export const EMBEDDING_MODEL_MAX_LENGTH = 200

/** The per-provider context token budget the server clamps a user's setting to, a mirror of
 * internal/service/chat_context.go (MaxContextTokenBudget); the Settings UI's field stops at it. */
export const CONTEXT_TOKEN_BUDGET_MAX = 128000

export const TOKEN_MAX_EXPIRY_DAYS = 365

export const CI_CREDENTIAL_MAX_EXPIRY_DAYS = 365

export const CI_CREDENTIAL_MAX_NAME = 100

/** AI behaviour caps, mirrors of internal/service/settings_validation.go (a Go test compares them). */
export const SYSTEM_PROMPT_SUFFIX_MAX_LENGTH = 4000

export const AI_PROMPTS_PER_LIST_MAX = 20

export const AI_PROMPT_MAX_LENGTH = 500

/** The per-provider Max tokens field's ceiling (MaxOutputTokensSetting). */
export const MAX_TOKENS_SETTING_MAX = 32000

/** A stored API key's length cap, a mirror of internal/service/credential_errors.go (MaxAPIKeyBytes). */
export const API_KEY_MAX_LENGTH = 8192
