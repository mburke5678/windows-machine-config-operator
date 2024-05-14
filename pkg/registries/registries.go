package registries

import (
	"fmt"
	"sort"
	"strings"

	config "github.com/openshift/api/config/v1"
)

// imagePathSeparator separates the repo name, namespaces, and image name in an OCI-compliant image name
const imagePathSeparator = "/"

// mirror represents a mirrored image repo entry in a registry configuration file
type mirror struct {
	// host is the mirror image location. Can include the registry hostname/IP address, port, and namespace path
	host string
	// resolveTags indicates to the container runtime if this mirror is allowed to resolve an image tag into a digest
	resolveTags bool
}

// newMirror constructs a new mirror object with proper host name structure to be used in containerd registry config
func newMirror(sourceImageLocation, mirrorImageLocation string, resolveTags bool) mirror {
	mirrorHost := ""
	// containerd appends any shared namespaces between source and mirror locations to the mirror's host entry in the
	// registry config file to construct the full mirror image location
	if sourceImageLocation != mirrorImageLocation {
		// truncate the mirror to drop any shared namespaces since containerd automatically appends them on image pull
		mirrorHost = extractMirrorURL(sourceImageLocation, mirrorImageLocation)
	} else {
		// special case if source and mirror are the same. Do not drop the host repo name to avoid an empty host entry
		mirrorHost = extractHostname(mirrorImageLocation)
	}
	return mirror{host: mirrorHost, resolveTags: resolveTags}
}

// extractMirrorURL drops the common suffix from the second repo, returning only the unique leading URL and namespaces
func extractMirrorURL(source, mirror string) string {
	sourceParts := strings.Split(source, imagePathSeparator)
	mirrorParts := strings.Split(mirror, imagePathSeparator)
	uniqueMirrorParts := mirrorParts

	// Process until the end of either repo string
	for i := 0; i < len(sourceParts) && i < len(mirrorParts); i++ {
		// Check if suffix piece is equal, starting from the backs of the lists
		if sourceParts[len(sourceParts)-1-i] != mirrorParts[len(mirrorParts)-1-i] {
			// break when something different is found to retain all pieces after the last common element
			break
		}
		// Remove common suffix piece
		uniqueMirrorParts = uniqueMirrorParts[:len(uniqueMirrorParts)-1]
	}
	return strings.Join(uniqueMirrorParts, imagePathSeparator)
}

// mirrorSet holds the mirror registry information for a single source image repo
type mirrorSet struct {
	// source is the image repo to be mirrored
	source string
	// mirrors represents mirrored repository locations to pull images from rather than the default source
	mirrors []mirror
	// mirrorSourcePolicy defines the fallback policy if fails to pull image from the mirrors
	mirrorSourcePolicy config.MirrorSourcePolicy
}

// newMirrorSet constructs an object with proper source and mirror name structures to be used in containerd registry config
func newMirrorSet(srcImage string, mirrorLocations []config.ImageMirror, resolveTags bool,
	mirrorSourcePolicy config.MirrorSourcePolicy) mirrorSet {
	truncatedMirrors := []mirror{}
	for _, m := range mirrorLocations {
		truncatedMirrors = append(truncatedMirrors, newMirror(srcImage, string(m), resolveTags))
	}
	return mirrorSet{source: extractHostname(srcImage), mirrors: truncatedMirrors, mirrorSourcePolicy: mirrorSourcePolicy}
}

// extractHostname extracts just the initial host repo from a full image location
// e.g. mcr.microsoft.com would be extracted from mcr.microsoft.com/oss/kubernetes/pause:3.9
func extractHostname(fullImage string) string {
	parts := strings.Split(fullImage, imagePathSeparator)
	return parts[0]
}

// registryConfig represents a system-wide image registry configuration
type registryConfig struct {
	sourceConfigs []mirrorSet
}

// NewRegistryConfig creates a new RegistryConfig object by extracting and merging the contents of the given mirror sets
func NewRegistryConfig(idmsItems []config.ImageDigestMirrorSet, idtsItems []config.ImageTagMirrorSet) *registryConfig {
	// Each member of the allMirrorSets collection represents the registry configuration for a specific source
	var allMirrorSets []mirrorSet

	for _, idms := range idmsItems {
		for _, entry := range idms.Spec.ImageDigestMirrors {
			set := newMirrorSet(entry.Source, entry.Mirrors, false, entry.MirrorSourcePolicy)
			allMirrorSets = append(allMirrorSets, set)
		}
	}
	for _, itms := range idtsItems {
		for _, entry := range itms.Spec.ImageTagMirrors {
			set := newMirrorSet(entry.Source, entry.Mirrors, true, entry.MirrorSourcePolicy)
			allMirrorSets = append(allMirrorSets, set)
		}
	}

	return &registryConfig{sourceConfigs: mergeMirrorSets(allMirrorSets)}
}

// mergeMirrorSets consolidates duplicate entries in the given slice (based on the source) since we do not want to
// generate multiple config files for the same source image repo. Output is sorted to ensure it is deterministic.
func mergeMirrorSets(baseMirrorSets []mirrorSet) []mirrorSet {
	// Map to keep track of unique mirrorSets by source
	uniqueMirrorSets := make(map[string]mirrorSet)

	for _, ms := range baseMirrorSets {
		if existingMS, ok := uniqueMirrorSets[ms.source]; ok {
			// If the source already exists, merge its mirrors slices
			existingMS.mirrors = mergeMirrors(existingMS.mirrors, ms.mirrors)
			// If the existing source's mirrorSourcePolicy conflicts, NeverContactSource is preferred
			if existingMS.mirrorSourcePolicy == config.AllowContactingSource {
				existingMS.mirrorSourcePolicy = ms.mirrorSourcePolicy
			}
			// Update the map entry
			uniqueMirrorSets[ms.source] = existingMS
		} else {
			// If it does not exist, add it to the map
			uniqueMirrorSets[ms.source] = ms
		}
	}

	// Convert the map back to a slice with no duplicates sources
	var result []mirrorSet
	for _, ms := range uniqueMirrorSets {
		result = append(result, ms)
	}

	sortMirrorSets(result)
	return result
}

// sortMirrorSets sorts the mirrorSets and each set of underlying mirrors aplhabetically. Modifies the parameter in place
func sortMirrorSets(mirrorSets []mirrorSet) {
	// Sort mirrors by host alphabetically within each mirrorSet
	for i := range mirrorSets {
		sort.Slice(mirrorSets[i].mirrors, func(j, k int) bool {
			return mirrorSets[i].mirrors[j].host < mirrorSets[i].mirrors[k].host
		})
	}
	// Sort mirrorSets by source alphabetically
	sort.Slice(mirrorSets, func(i, j int) bool {
		return mirrorSets[i].source < mirrorSets[j].source
	})
}

// mergeMirrors consolidates duplicate mirrors in the given slice (based on the host) since we do not want to
// generate multiple entries in a single config file for the same mirror repo
func mergeMirrors(existingMirrors, newMirrors []mirror) []mirror {
	// Map to keep track of unique mirrors by host
	uniqueMirrors := make(map[string]mirror)

	// Iterate over existing mirrors and add them to the map
	for _, m := range existingMirrors {
		uniqueMirrors[m.host] = m
	}
	// Iterate over new mirrors
	for _, m := range newMirrors {
		if existingM, ok := uniqueMirrors[m.host]; ok {
			// If the mirror already exists, check the resolveTags field. Resolving by tag is preferred over by digest.
			if !existingM.resolveTags && m.resolveTags {
				uniqueMirrors[m.host] = m
			}
		} else {
			// If the mirror does not exist, add it to the map
			uniqueMirrors[m.host] = m
		}
	}

	// Convert the map back to a slice with no duplicates mirrors
	var result []mirror
	for _, m := range uniqueMirrors {
		result = append(result, m)
	}
	return result
}

// generateConfig is a serialization method that generates a valid TOML representation from a mirrorSet object.
// Results in content usable as a containerd image registry configuration file. Returns empty string if no mirrors exist
func (ms *mirrorSet) generateConfig() string {
	if len(ms.mirrors) == 0 {
		return ""
	}

	result := ""

	fallbackServer := ms.source
	if ms.mirrorSourcePolicy == config.NeverContactSource {
		// set the fallback server to the first mirror to ensure the source is never contacted, even if all mirrors fail
		fallbackServer = ms.mirrors[0].host
	}
	result += fmt.Sprintf("server = \"https://%s\"", fallbackServer)
	result += "\r\n\r\n"

	// Each mirror should result in an entry followed by a set of settings for interacting with the mirror host
	for _, m := range ms.mirrors {
		result += fmt.Sprintf("[host.\"https://%s\"]", m.host)
		result += "\r\n"

		// Specify the operations the registry host may perform. IDMS mirrors can only be pulled by directly by digest,
		// whereas ITMS mirrors have the additional resolve capability, which allows converting a tag name into a digest
		var hostCapabilities string
		if m.resolveTags {
			hostCapabilities = "  capabilities = [\"pull\", \"resolve\"]"
		} else {
			hostCapabilities = "  capabilities = [\"pull\"]"
		}
		result += hostCapabilities
		result += "\r\n"
	}

	return result
}
