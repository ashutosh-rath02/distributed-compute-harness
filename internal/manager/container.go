package manager

import (
	"fmt"
	"strconv"
	"strings"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Container tasks (roadmap item 13): container.run is opt-in (off until
// policy turns it on, catalog.Type.OptIn) and its policy entry also says
// which images may run, whether by tag, and whether with a network. Those
// are checked on the compiled parameters wherever policy is: at submit
// (API, job attempts), when a job or plan is checked up front, and before
// a restart.

const maxAllowedImages = 64

// canonContainerPolicy checks the container fields of capability's policy
// entry and puts its allowed images in canonical form.
func canonContainerPolicy(capability domain.CapabilityName, tp *domain.TypePolicy) error {
	if capability != catalog.ContainerRun {
		if len(tp.AllowImages) > 0 || tp.AllowTags || tp.AllowNetwork {
			return fmt.Errorf("%s: allowImages, allowTags and allowNetwork apply to %s only", capability, catalog.ContainerRun)
		}
		return nil
	}
	if len(tp.AllowImages) > maxAllowedImages {
		return fmt.Errorf("%s: at most %d allowed images", capability, maxAllowedImages)
	}
	seen := map[string]bool{}
	var images []string
	for _, e := range tp.AllowImages {
		c, err := catalog.CanonAllowedImage(e)
		if err != nil {
			return fmt.Errorf("%s: %v", capability, err)
		}
		if !seen[c] {
			seen[c] = true
			images = append(images, c)
		}
	}
	tp.AllowImages = images
	return nil
}

// checkContainer applies container.run's policy to a compiled submission
// (other capabilities pass) and, given req, raises the reservation to the
// container's own limits, so placement counts what it may use.
func (s *Server) checkContainer(capability domain.CapabilityName, params map[string]string, req *domain.ResourceRequirements) error {
	if capability != catalog.ContainerRun {
		return nil
	}
	tp := s.policyFor(capability)
	img, err := catalog.ParseImage(params["image"])
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalidWorkload, capability, err)
	}
	if !img.Pinned() && !tp.AllowTags {
		return fmt.Errorf("%w: %s runs only images pinned by digest (name@sha256:...), since a tag can change what runs: pin %s, or allow tags with \"harnessctl policy type %s tags on\"",
			ErrPolicy, capability, img, capability)
	}
	if !catalog.ImageAllowed(img, tp.AllowImages) {
		ns := img.Repo[:strings.LastIndex(img.Repo, "/")]
		return fmt.Errorf("%w: %s may not run %s: it isn't one of the allowed images — allow it with \"harnessctl policy type %s images add %s\" (or every image under %s/ with \"... images add %s/*\"), or in the dashboard's Policy section",
			ErrPolicy, capability, img, capability, img, ns, ns)
	}
	if params["network"] == "bridge" && !tp.AllowNetwork {
		return fmt.Errorf("%w: %s with network=bridge needs network access allowed: \"harnessctl policy type %s network on\" (or run it with network=none)",
			ErrPolicy, capability, capability)
	}
	if req != nil {
		if mb, err := strconv.Atoi(params["memory_mb"]); err == nil && uint64(mb)<<20 > req.MinMemoryBytes {
			req.MinMemoryBytes = uint64(mb) << 20
		}
		if cpus, err := strconv.ParseFloat(params["cpus"], 64); err == nil && cpus > req.MinCPUCores {
			req.MinCPUCores = cpus
		}
	}
	return nil
}

// restartAllowed is checkPolicy plus the container rules, for a restart:
// policy may have changed since the workload was submitted.
func (s *Server) restartAllowed(w domain.Workload) error {
	if err := s.checkPolicy(w.EffectiveCapability()); err != nil {
		return err
	}
	return s.checkContainer(w.EffectiveCapability(), w.Params, nil)
}
