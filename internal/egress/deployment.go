package egress

import "pad-analyzer/internal/capability"

// ForDeployment is the deployment's one answer to "may a tenant-configured target reach this server's machine or
// private network" (ADR 0021): with accounts there is more than one user, so another user's URL is untrusted input
// and internal targets are refused; without accounts the single user's webhook on their own machine is what they
// meant, and loopback is allowed. The organisation channels' validation, the scanner's deliveries to them and the
// channel test all use it (B8: three predicates used to disagree in local mode with accounts). The AI provider
// endpoints keep their own until Phase 4b.
func ForDeployment(caps capability.Set) Policy {
	if caps.Has(capability.Accounts) {
		return BlockInternal
	}
	return AllowLoopback
}

// ForOperator is the policy of the deployment's own channels (PAD_NOTIFY_*): the operator's configuration, not a
// tenant's input, so accounts do not decide. Outside the cloud a receiver on the operator's machine is allowed (local
// mode with accounts is a development profile, where a localhost receiver is how a webhook is tried out); the cloud
// refuses internal targets, as it always has.
func ForOperator(caps capability.Set) Policy {
	if caps.Profile() == capability.Cloud {
		return BlockInternal
	}
	return AllowLoopback
}
