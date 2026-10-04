package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"home-harness/internal/catalog"
	"home-harness/internal/domain"
)

// Container policy (internal/manager/container.go): which images
// container.run may run, whether by tag, and whether with a network.

const containerPolicyUsage = "harnessctl policy type container.run images add|remove REF | images set REF,... | images - | tags on|off | network on|off"

// setContainerPolicy applies one container setting to tp ("images",
// "tags", "network"); any other setting is unknown.
func setContainerPolicy(name domain.CapabilityName, tp *domain.TypePolicy, setting string, args []string) error {
	switch setting {
	case "images", "tags", "network":
	default:
		return fmt.Errorf("unknown policy setting %q", setting)
	}
	if name != catalog.ContainerRun {
		return fmt.Errorf("%s applies to %s only", setting, catalog.ContainerRun)
	}
	if len(args) == 0 {
		return errors.New("usage: " + containerPolicyUsage)
	}
	switch setting {
	case "tags", "network":
		if args[0] != "on" && args[0] != "off" {
			return errors.New("usage: " + containerPolicyUsage)
		}
		if setting == "tags" {
			tp.AllowTags = args[0] == "on"
		} else {
			tp.AllowNetwork = args[0] == "on"
		}
		return nil
	}
	canon := func(refs string) ([]string, error) {
		var out []string
		for _, r := range strings.Split(refs, ",") {
			if r = strings.TrimSpace(r); r == "" {
				continue
			}
			c, err := catalog.CanonAllowedImage(r)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	}
	switch {
	case args[0] == "-":
		tp.AllowImages = nil
	case (args[0] == "add" || args[0] == "remove" || args[0] == "set") && len(args) == 2:
		refs, err := canon(args[1])
		if err != nil {
			return err
		}
		switch args[0] {
		case "set":
			tp.AllowImages = refs
		case "add":
			for _, r := range refs {
				if !slices.Contains(tp.AllowImages, r) {
					tp.AllowImages = append(tp.AllowImages, r)
				}
			}
		case "remove":
			before := len(tp.AllowImages)
			tp.AllowImages = slices.DeleteFunc(tp.AllowImages, func(e string) bool { return slices.Contains(refs, e) })
			if len(tp.AllowImages) == before {
				return fmt.Errorf("%s is not among the allowed images", args[1])
			}
		}
	default:
		return errors.New("usage: " + containerPolicyUsage)
	}
	return nil
}

// containerPolicyLine describes tp's container settings for display.
func containerPolicyLine(tp domain.TypePolicy) string {
	images := "none (add with: harnessctl policy type container.run images add REF)"
	if len(tp.AllowImages) > 0 {
		images = strings.Join(tp.AllowImages, ", ")
	}
	return fmt.Sprintf("Allowed images %s; by tag %s; network %s", images, onOff(tp.AllowTags), onOff(tp.AllowNetwork))
}
