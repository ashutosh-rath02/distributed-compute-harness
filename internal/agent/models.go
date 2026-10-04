package agent

import "home-harness/internal/domain"

// changesModels reports whether a finished task of capability may have
// changed the device's models (a download or a removal).
func changesModels(capability domain.CapabilityName) bool {
	return capability == "llm.pull" || capability == "llm.remove"
}

// requestReprobe has the capability probe look again now (once; a probe
// already asked for is enough).
func (a *Agent) requestReprobe() {
	select {
	case a.reprobe <- struct{}{}:
	default:
	}
}
