package ai

import (
	"strings"

	"pad-core/cache"
)

// The provider memos (R-provider-31): what the providers learn from an upstream's answers, held in declared caches
// (ADR 0017) so each is bounded by least-recently-used eviction at its cap (never a wholesale reset or a random
// delete), counted in pad_cache_* by name, and listed in the caches golden. Each memo's own file builds its storage
// with cache.New and keeps its TTL check on the entry's own timestamp, which the cache's TTL backs.
//
// The upstream memos are keyed by an upstream endpoint and deployment (memoKey): a fact about the deployment, the same
// for every caller of it, which only shapes the next request to it and never reaches an answer or a model list.
var (
	paramRepairsSpec = cache.Declare("ai.param-repairs",
		cache.Keys(cache.KindParam),
		cache.Global("keyed by the upstream endpoint and model: a parameter the deployment refuses is a fact about the deployment, the same for every caller; it only shapes the next request"),
		cache.ReplicaLocal("learned from an upstream rejection; another replica pays the one rejection round trip and learns it too (1 h TTL)"))
	streamRefusalsSpec = cache.Declare("ai.stream-refusals",
		cache.Keys(cache.KindParam),
		cache.Global("keyed by the upstream endpoint and deployment: a model that refuses streaming refuses it for every caller"),
		cache.ReplicaLocal("learned from an upstream rejection; another replica pays the one rejection round trip and learns it too (1 h TTL)"))
	responsesToolRoutesSpec = cache.Declare("ai.responses-tool-routes",
		cache.Keys(cache.KindParam),
		cache.Global("keyed by the resource's Responses route and deployment: whether the route serves the deployment is the same for every caller"),
		cache.ReplicaLocal("learned from the upstream's answer; another replica pays the one refused or rerouted round trip and learns it too (1 h TTL)"))
	embedDimsRefusalsSpec = cache.Declare("ai.embed-dims-refusals",
		cache.Keys(cache.KindParam),
		cache.Global("keyed by the embeddings endpoint and model: a model that refuses `dimensions` refuses it for every caller"),
		cache.ReplicaLocal("learned from an upstream rejection; another replica pays the one rejection round trip and learns it too (24 h TTL)"))
	agentKindsSpec = cache.Declare("ai.agent-kinds",
		cache.Keys(cache.KindParam),
		cache.Global("keyed by the project endpoint and agent name: the kind the project's listing reported, the same for every caller of the project; it only decides the workflow opt-in header"),
		cache.ReplicaLocal("filled by this replica's project listings; another replica learns the kind from its own listing (24 h TTL)"))
	// The served-model memo and the learned windows are per credential scope (a user's own, an org's shared one, or
	// the local user's) and resource: one user's observation never reaches another's model list.
	servedModelsSpec = cache.Declare("ai.served-models",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindParam, cache.KindParam),
		cache.ReplicaLocal("alias to served model, learned from responses; another replica relearns it from its next answer (24 h TTL)"))
	servedByResourceSpec = cache.Declare("ai.served-by-resource",
		cache.Keys(cache.KindParam, cache.KindParam, cache.KindParam),
		cache.Global("keyed by resource and deployment: a deployment serves the same model for every caller of its resource; the answer only shapes the request (the Claude route), never a model list"),
		cache.ReplicaLocal("the resource-wide view of ai.served-models, learned from responses; another replica relearns it from its next answer (24 h TTL)"))
	contextWindowsSpec = cache.Declare("ai.context-windows",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindParam, cache.KindParam),
		cache.ReplicaLocal("context windows learned from a provider's rejection; another replica pays one rejected call and learns it too (24 h TTL)"))
)

// memoKey is an upstream memo's key: the opaque endpoint + deployment string its callers build.
func memoKey(spec *cache.Spec, key string) cache.Key {
	k, _ := spec.Key(cache.Param("upstream", key))
	return k
}

// scopedMemoKey is the key of a per-scope memo (ai.served-models, ai.context-windows): the scope's credential owner as
// the tenant, its resource, the provider and the requested model. A scope is the credential scope (a user id,
// OrgScope(org) or "" for the local user), followed for an alias provider by NUL and the resource.
func scopedMemoKey(spec *cache.Spec, scope, providerID, requestModel string) cache.Key {
	owner, resource, _ := strings.Cut(scope, "\x00")
	tenant := cache.Local()
	if org, ok := strings.CutPrefix(owner, OrgScope("")); ok && org != "" {
		tenant = cache.Org(org)
	} else if owner != "" {
		tenant = cache.User(owner)
	}
	k, _ := spec.Key(tenant, cache.Param("resource", resource), cache.Param("provider", providerID), cache.Param("model", requestModel))
	return k
}
