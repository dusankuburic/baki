
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

export const TOKEN_MAX_EXPIRY_DAYS = 365

export const CI_CREDENTIAL_MAX_EXPIRY_DAYS = 365

export const CI_CREDENTIAL_MAX_NAME = 100
