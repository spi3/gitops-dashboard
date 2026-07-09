package core

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var semverTagPattern = regexp.MustCompile(`^v?([0-9]+)(?:\.([0-9]+)(?:\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?)?)?$`)

func ParseImageReference(raw string) ImageReference {
	original := strings.TrimSpace(raw)
	ref := ImageReference{Original: original}
	if original == "" {
		return ref
	}
	value := stripImageScheme(original)
	if isBareDigest(value) {
		ref.Digest = value
		return ref
	}
	name := value
	if at := strings.LastIndex(name, "@"); at >= 0 {
		ref.Digest = strings.TrimSpace(name[at+1:])
		name = strings.TrimSpace(name[:at])
	}
	lastSlash := strings.LastIndex(name, "/")
	lastColon := strings.LastIndex(name, ":")
	if lastColon > lastSlash {
		ref.Tag = strings.TrimSpace(name[lastColon+1:])
		name = strings.TrimSpace(name[:lastColon])
	}
	if name == "" {
		return ref
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && isRegistryComponent(parts[0]) {
		ref.Registry = parts[0]
		ref.Repository = strings.Join(parts[1:], "/")
	} else {
		ref.Repository = name
	}
	return ref
}

func ImageReferences(values []string) []ImageReference {
	refs := make([]ImageReference, 0, len(values))
	for _, value := range values {
		ref := ParseImageReference(value)
		if ref.Original == "" {
			continue
		}
		refs = append(refs, ref)
	}
	return refs
}

func NewObservedImage(target, runtime, image, imageID string, repoDigests []string) ObservedImage {
	observed := ObservedImage{
		Target:      strings.TrimSpace(target),
		Runtime:     strings.TrimSpace(runtime),
		Reference:   ParseImageReference(image),
		ImageID:     strings.TrimSpace(imageID),
		RepoDigests: ImageReferences(repoDigests),
	}
	if imageIDRef := ParseImageReference(imageID); imageIDRef.Digest != "" && imageIDRef.Repository != "" {
		observed.RepoDigests = append(observed.RepoDigests, imageIDRef)
	}
	observed.RepoDigests = uniqueImageReferences(observed.RepoDigests)
	return observed
}

func MutableImageWarnings(images []string) []string {
	refs := ImageReferences(images)
	warnings := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !ref.Mutable() {
			continue
		}
		version := ref.Tag
		if version == "" {
			version = "no tag"
		}
		warnings = append(warnings, fmt.Sprintf("image %s uses mutable reference (%s)", ref.Original, version))
	}
	sort.Strings(warnings)
	return warnings
}

func ApplyImageVersionComparisons(services []Service, statuses []StatusResult) {
	statusesByService := map[string][]StatusResult{}
	for _, status := range statuses {
		statusesByService[status.ServiceID] = append(statusesByService[status.ServiceID], status)
	}
	for i := range services {
		NormalizeServiceImageMetadata(&services[i])
		services[i].ImageVersionState, services[i].ImageVersionChecks = CompareServiceImages(services[i], statusesByService[services[i].ID])
	}
}

func NormalizeServiceImageMetadata(service *Service) {
	service.DesiredImages = ImageReferences(service.Images)
	if service.ImageVersionState == "" {
		service.ImageVersionState = ImageVersionUnknown
	}
	if service.ImageVersionChecks == nil {
		service.ImageVersionChecks = []ImageVersionCheck{}
	}
}

func CompareServiceImages(service Service, statuses []StatusResult) (ImageVersionState, []ImageVersionCheck) {
	desired := service.DesiredImages
	if len(desired) == 0 {
		desired = ImageReferences(service.Images)
	}
	desired = uniqueImageReferences(desired)
	if len(desired) == 0 {
		return ImageVersionUnknown, []ImageVersionCheck{}
	}
	observed := observedImages(statuses)
	checks := make([]ImageVersionCheck, 0, len(desired))
	for _, ref := range desired {
		checks = append(checks, initialImageVersionCheck(ref, len(observed) == 0))
	}
	usedObserved := make([]bool, len(observed))

	for i := range checks {
		if match, observedIndex, ok := exactObservedImage(checks[i].Desired, observed, usedObserved); ok {
			fillImageVersionCheck(&checks[i], match)
			usedObserved[observedIndex] = true
		}
	}
	for i := range checks {
		if checks[i].Observed != nil {
			continue
		}
		if match, observedIndex, ok := repositoryObservedImage(checks[i].Desired, observed, usedObserved); ok {
			fillImageVersionCheck(&checks[i], match)
			usedObserved[observedIndex] = true
		}
	}
	for observedIndex, item := range observed {
		if observedIndex < len(usedObserved) && usedObserved[observedIndex] {
			continue
		}
		if ref, ok := desiredForObservedImage(desired, item); ok {
			check := initialImageVersionCheck(ref, false)
			fillImageVersionCheck(&check, item)
			checks = append(checks, check)
		}
	}
	return aggregateImageVersionState(checks), checks
}

func initialImageVersionCheck(desired ImageReference, noObserved bool) ImageVersionCheck {
	check := ImageVersionCheck{Desired: desired, State: ImageVersionUnknown}
	if desired.Mutable() {
		check.State = ImageVersionMutable
		check.Message = "desired image is mutable; pin a SemVer tag or digest"
		return check
	}
	if noObserved {
		check.Message = "no observed image reported"
	} else {
		check.Message = "no observed image reported for desired repository"
	}
	return check
}

func fillImageVersionCheck(check *ImageVersionCheck, observed ObservedImage) {
	check.Observed = &observed
	if check.Desired.Mutable() {
		check.State = ImageVersionMutable
		check.Message = "desired image is mutable; pin a SemVer tag or digest"
		return
	}
	matches, known := imageReferencesMatch(check.Desired, observed)
	switch {
	case !known:
		check.State = ImageVersionUnknown
		check.Message = "observed image does not include comparable tag or digest metadata"
	case matches:
		check.State = ImageVersionMatching
		check.Message = "desired and observed image versions match"
	default:
		check.State = ImageVersionMismatched
		check.Message = "desired and observed image versions differ"
	}
}

func imageReferencesMatch(desired ImageReference, observed ObservedImage) (bool, bool) {
	observedRefs := observedComparableReferences(observed)
	if desired.Digest != "" {
		knownDigest := false
		for _, ref := range observedRefs {
			if ref.Digest == "" {
				continue
			}
			knownDigest = true
			if repositoryMatches(desired, ref) && digestEqual(desired.Digest, ref.Digest) {
				return true, true
			}
		}
		return false, knownDigest
	}
	if desired.Tag == "" {
		return false, false
	}
	knownTag := false
	for _, ref := range observedRefs {
		if ref.Tag == "" {
			continue
		}
		if !repositoryMatches(desired, ref) {
			continue
		}
		knownTag = true
		if strings.EqualFold(desired.Tag, ref.Tag) {
			return true, true
		}
	}
	return false, knownTag
}

func observedComparableReferences(observed ObservedImage) []ImageReference {
	refs := []ImageReference{}
	if observed.Reference.Original != "" {
		refs = append(refs, observed.Reference)
	}
	refs = append(refs, observed.RepoDigests...)
	return uniqueImageReferences(refs)
}

func exactObservedImage(desired ImageReference, observed []ObservedImage, usedObserved []bool) (ObservedImage, int, bool) {
	for index, item := range observed {
		if index < len(usedObserved) && usedObserved[index] {
			continue
		}
		matches, known := imageReferencesMatch(desired, item)
		if known && matches {
			return item, index, true
		}
	}
	return ObservedImage{}, -1, false
}

func repositoryObservedImage(desired ImageReference, observed []ObservedImage, usedObserved []bool) (ObservedImage, int, bool) {
	for index, item := range observed {
		if index < len(usedObserved) && usedObserved[index] {
			continue
		}
		if repositoryMatches(desired, item.Reference) {
			return item, index, true
		}
		for _, digest := range item.RepoDigests {
			if repositoryMatches(desired, digest) {
				return item, index, true
			}
		}
	}
	return ObservedImage{}, -1, false
}

func desiredForObservedImage(desired []ImageReference, observed ObservedImage) (ImageReference, bool) {
	for _, ref := range desired {
		if repositoryMatches(ref, observed.Reference) {
			return ref, true
		}
		for _, digest := range observed.RepoDigests {
			if repositoryMatches(ref, digest) {
				return ref, true
			}
		}
	}
	return ImageReference{}, false
}

func observedImages(statuses []StatusResult) []ObservedImage {
	var result []ObservedImage
	for _, status := range statuses {
		if status.Health == HealthNotApplicable {
			continue
		}
		for _, image := range status.ObservedImages {
			if image.Target == "" {
				image.Target = status.Target
			}
			result = append(result, image)
		}
	}
	return uniqueObservedImages(result)
}

func aggregateImageVersionState(checks []ImageVersionCheck) ImageVersionState {
	if len(checks) == 0 {
		return ImageVersionUnknown
	}
	state := ImageVersionMatching
	for _, check := range checks {
		if imageVersionPriority(check.State) < imageVersionPriority(state) {
			state = check.State
		}
	}
	return state
}

func imageVersionPriority(state ImageVersionState) int {
	switch state {
	case ImageVersionMismatched:
		return 0
	case ImageVersionMutable:
		return 1
	case ImageVersionUnknown:
		return 2
	case ImageVersionMatching:
		return 3
	default:
		return 2
	}
}

func (ref ImageReference) Mutable() bool {
	if ref.Original == "" || ref.Digest != "" {
		return false
	}
	tag := strings.TrimSpace(ref.Tag)
	if tag == "" || strings.EqualFold(tag, "latest") {
		return true
	}
	// Moving release-channel tags and unknown tag schemes are mutable by
	// default; only full SemVer tags and digest-pinned references are immutable.
	return !isFullSemVerTag(tag)
}

func isFullSemVerTag(tag string) bool {
	match := semverTagPattern.FindStringSubmatch(strings.TrimSpace(tag))
	if match == nil || match[3] == "" {
		return false
	}
	for _, value := range match[1:4] {
		if !validSemVerNumber(value) {
			return false
		}
	}
	return validPrereleaseIdentifiers(match[4])
}

func validSemVerNumber(value string) bool {
	return value == "0" || (value != "" && !strings.HasPrefix(value, "0"))
}

func validPrereleaseIdentifiers(value string) bool {
	if value == "" {
		return true
	}
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" {
			return false
		}
		if allDigits(identifier) && !validSemVerNumber(identifier) {
			return false
		}
	}
	return true
}

func allDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return value != ""
}

func repositoryMatches(left, right ImageReference) bool {
	leftRepo := normalizedRepository(left)
	rightRepo := normalizedRepository(right)
	if leftRepo == "" || rightRepo == "" {
		return false
	}
	if leftRepo == rightRepo {
		return true
	}
	return false
}

func normalizedRepository(ref ImageReference) string {
	repository := strings.Trim(strings.ToLower(ref.Repository), "/")
	if repository == "" {
		return ""
	}
	registry := strings.Trim(strings.ToLower(ref.Registry), "/")
	switch registry {
	case "", "index.docker.io":
		registry = "docker.io"
	}
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return registry + "/" + repository
}

func digestEqual(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func stripImageScheme(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"docker-pullable://", "docker://", "containerd://"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return value
}

func isBareDigest(value string) bool {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "/") || strings.Contains(value, "@") {
		return false
	}
	algorithm, digest, ok := strings.Cut(value, ":")
	if !ok || digest == "" {
		return false
	}
	switch algorithm {
	case "sha256", "sha384", "sha512":
		return true
	default:
		return false
	}
}

func isRegistryComponent(value string) bool {
	return strings.Contains(value, ".") || strings.Contains(value, ":") || value == "localhost"
}

func uniqueImageReferences(refs []ImageReference) []ImageReference {
	seen := map[string]struct{}{}
	result := make([]ImageReference, 0, len(refs))
	for _, ref := range refs {
		key := strings.Join([]string{ref.Original, ref.Registry, ref.Repository, ref.Tag, ref.Digest}, "\x00")
		if key == "\x00\x00\x00\x00" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, ref)
	}
	return result
}

func uniqueObservedImages(images []ObservedImage) []ObservedImage {
	seen := map[string]struct{}{}
	result := make([]ObservedImage, 0, len(images))
	for _, image := range images {
		key := strings.Join([]string{
			image.Target,
			image.Runtime,
			image.Reference.Original,
			image.ImageID,
			fmt.Sprint(image.RepoDigests),
		}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if image.RepoDigests == nil {
			image.RepoDigests = []ImageReference{}
		}
		result = append(result, image)
	}
	return result
}
