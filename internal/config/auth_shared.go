package config

import "pad-analyzer/internal/platform/secret"

// SharedSecret is the secret every replica of a deployment holds and that survives a restart, for what must verify
// across replicas and processes (the HMAC key of the Redis command bus): the dedicated PAD_ENCRYPTION_KEY, else the
// operator's PAD_AUTH_SECRET (the same choice as the database keystore's key). A secret minted for this process
// because none was configured never qualifies: the other replicas do not have it. Unset when there is none.
func (a AuthConfig) SharedSecret() secret.Value {
	if a.EncryptionKey.IsSet() {
		return a.EncryptionKey
	}
	if a.SecretEphemeral {
		return secret.Value{}
	}
	return a.Secret
}
