package core

import "testing"

func TestParseImageReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw        string
		registry   string
		repository string
		tag        string
		digest     string
		mutable    bool
	}{
		{
			raw:        "ghcr.io/spi3/gitops-dashboard:v1.2.3@sha256:abc123",
			registry:   "ghcr.io",
			repository: "spi3/gitops-dashboard",
			tag:        "v1.2.3",
			digest:     "sha256:abc123",
		},
		{
			raw:        "localhost:5000/team/api:main",
			registry:   "localhost:5000",
			repository: "team/api",
			tag:        "main",
			mutable:    true,
		},
		{
			raw:        "nginx:latest",
			repository: "nginx",
			tag:        "latest",
			mutable:    true,
		},
		{
			raw:        "docker-pullable://docker.io/library/nginx@sha256:def456",
			registry:   "docker.io",
			repository: "library/nginx",
			digest:     "sha256:def456",
		},
		{
			raw:     "sha256:deadbeef",
			digest:  "sha256:deadbeef",
			mutable: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			ref := ParseImageReference(tc.raw)
			if ref.Registry != tc.registry || ref.Repository != tc.repository || ref.Tag != tc.tag || ref.Digest != tc.digest {
				t.Fatalf("ref = %#v", ref)
			}
			if ref.Mutable() != tc.mutable {
				t.Fatalf("Mutable() = %v, want %v", ref.Mutable(), tc.mutable)
			}
		})
	}
}

func TestImageReferenceMutableTags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw     string
		mutable bool
	}{
		{raw: "example/api", mutable: true},
		{raw: "example/api:latest", mutable: true},
		{raw: "example/api:Latest", mutable: true},
		{raw: "example/api:v1", mutable: true},
		{raw: "example/api:1", mutable: true},
		{raw: "example/api:v1.2", mutable: true},
		{raw: "example/api:1.2", mutable: true},
		{raw: "example/api:main", mutable: true},
		{raw: "example/api:sha-abc123", mutable: true},
		{raw: "example/api:v1.2.3", mutable: false},
		{raw: "example/api:1.2.3", mutable: false},
		{raw: "example/api:v0.1.0", mutable: false},
		{raw: "example/api:v1.2.3-alpha.1", mutable: false},
		{raw: "example/api:v1.2.3+build.7", mutable: false},
		{raw: "example/api:v1.2.3-alpha.1+build.7", mutable: false},
		{raw: "example/api:v1.2.3@sha256:release", mutable: false},
		{raw: "example/api:v1@sha256:release", mutable: false},
		{raw: "example/api:main@sha256:release", mutable: false},
		{raw: "example/api:v01.2.3", mutable: true},
		{raw: "example/api:v1.02.3", mutable: true},
		{raw: "example/api:v1.2.03", mutable: true},
		{raw: "example/api:v1.2.3-01", mutable: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			if got := ParseImageReference(tc.raw).Mutable(); got != tc.mutable {
				t.Fatalf("Mutable() = %v, want %v", got, tc.mutable)
			}
		})
	}
}

func TestCompareServiceImages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		desired  []string
		observed []ObservedImage
		want     ImageVersionState
	}{
		{
			name:    "matching tag",
			desired: []string{"example/api:v1.0.0"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "example/api:v1.0.0", "sha256:local", nil),
			},
			want: ImageVersionMatching,
		},
		{
			name:    "mismatched tag",
			desired: []string{"example/api:v1.0.0"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "example/api:v2.0.0", "sha256:local", nil),
			},
			want: ImageVersionMismatched,
		},
		{
			name:    "cross registry unknown",
			desired: []string{"ghcr.io/acme/api:v1.0.0"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "docker.io/acme/api:v1.0.0", "sha256:local", nil),
			},
			want: ImageVersionUnknown,
		},
		{
			name:    "matching digest",
			desired: []string{"example/api@sha256:release"},
			observed: []ObservedImage{
				NewObservedImage("cluster", "kubernetes", "example/api:v1", "docker-pullable://example/api@sha256:release", nil),
			},
			want: ImageVersionMatching,
		},
		{
			name:    "unknown without runtime",
			desired: []string{"example/api:v1.0.0"},
			want:    ImageVersionUnknown,
		},
		{
			name:    "latest desired is mutable",
			desired: []string{"example/api:latest"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "example/api:latest", "", nil),
			},
			want: ImageVersionMutable,
		},
		{
			name:    "release channel desired is mutable",
			desired: []string{"example/api:v1.2"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "example/api:v1.2", "", nil),
			},
			want: ImageVersionMutable,
		},
		{
			name:    "unknown tag desired is mutable",
			desired: []string{"example/api:main"},
			observed: []ObservedImage{
				NewObservedImage("docker", "docker", "example/api:main", "", nil),
			},
			want: ImageVersionMutable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service := Service{ID: "svc", Images: tc.desired}
			state, checks := CompareServiceImages(service, []StatusResult{{
				ServiceID:      "svc",
				Target:         "runtime",
				Health:         HealthHealthy,
				ObservedImages: tc.observed,
			}})
			if state != tc.want {
				t.Fatalf("state = %s, want %s; checks=%#v", state, tc.want, checks)
			}
			if len(checks) != len(tc.desired) {
				t.Fatalf("checks = %#v, want one per desired image", checks)
			}
		})
	}
}

func TestCompareServiceImagesPrefersExactObservedPairing(t *testing.T) {
	t.Parallel()
	service := Service{ID: "svc", Images: []string{"example/api:v1.0.0", "example/api:v2.0.0"}}
	state, checks := CompareServiceImages(service, []StatusResult{{
		ServiceID: "svc",
		Target:    "runtime",
		Health:    HealthHealthy,
		ObservedImages: []ObservedImage{
			NewObservedImage("runtime", "docker", "example/api:v2.0.0", "sha256:v2", nil),
			NewObservedImage("runtime", "docker", "example/api:v1.0.0", "sha256:v1", nil),
		},
	}})
	if state != ImageVersionMatching {
		t.Fatalf("state = %s, want matching; checks=%#v", state, checks)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %#v, want two checks", checks)
	}
	for _, check := range checks {
		if check.Observed == nil {
			t.Fatalf("check = %#v, want observed image", check)
		}
		if check.Desired.Tag != check.Observed.Reference.Tag {
			t.Fatalf("desired %s paired with observed %s", check.Desired.Tag, check.Observed.Reference.Tag)
		}
	}
}

func TestCompareServiceImagesDedupesDesiredReferences(t *testing.T) {
	t.Parallel()
	service := Service{ID: "svc", Images: []string{"example/api:v1.0.0", "example/api:v1.0.0"}}
	state, checks := CompareServiceImages(service, []StatusResult{{
		ServiceID: "svc",
		Target:    "runtime",
		Health:    HealthHealthy,
		ObservedImages: []ObservedImage{
			NewObservedImage("runtime", "docker", "example/api:v1.0.0", "sha256:api", nil),
		},
	}})
	if state != ImageVersionMatching {
		t.Fatalf("state = %s, want matching; checks=%#v", state, checks)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %#v, want single comparison for duplicate desired refs", checks)
	}
	if checks[0].State != ImageVersionMatching || checks[0].Observed == nil {
		t.Fatalf("check = %#v, want matching observed image", checks[0])
	}
}

func TestCompareServiceImagesSkipsNotApplicableObservedImages(t *testing.T) {
	t.Parallel()
	service := Service{ID: "svc", Images: []string{"example/api:v1.0.0"}}
	state, checks := CompareServiceImages(service, []StatusResult{{
		ServiceID: "svc",
		Target:    "docker",
		Health:    HealthNotApplicable,
		ObservedImages: []ObservedImage{
			NewObservedImage("docker", "docker", "example/api:v1.0.0", "sha256:api", nil),
		},
	}})
	if state != ImageVersionUnknown {
		t.Fatalf("state = %s, want unknown without applicable observed images; checks=%#v", state, checks)
	}
	if len(checks) != 1 || checks[0].Observed != nil || checks[0].State != ImageVersionUnknown {
		t.Fatalf("checks = %#v, want unknown check without observed image", checks)
	}
}

func TestCompareServiceImagesDoesNotConsumeUnrelatedObservedImage(t *testing.T) {
	t.Parallel()
	service := Service{ID: "svc", Images: []string{"example/api:v1.0.0", "example/worker:v1.0.0"}}
	state, checks := CompareServiceImages(service, []StatusResult{{
		ServiceID: "svc",
		Target:    "runtime",
		Health:    HealthHealthy,
		ObservedImages: []ObservedImage{
			NewObservedImage("runtime", "docker", "example/worker:v1.0.0", "sha256:worker", nil),
		},
	}})
	if state != ImageVersionUnknown {
		t.Fatalf("state = %s, want unknown for missing api with matching worker; checks=%#v", state, checks)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %#v, want one check per desired image", checks)
	}
	byRepository := map[string]ImageVersionCheck{}
	for _, check := range checks {
		byRepository[check.Desired.Repository] = check
	}
	if byRepository["example/api"].Observed != nil || byRepository["example/api"].State != ImageVersionUnknown {
		t.Fatalf("api check = %#v, want unknown without cross-repository observed pairing", byRepository["example/api"])
	}
	if byRepository["example/worker"].Observed == nil || byRepository["example/worker"].State != ImageVersionMatching {
		t.Fatalf("worker check = %#v, want exact observed match", byRepository["example/worker"])
	}
}

func TestCompareServiceImagesReportsMixedObservedVersions(t *testing.T) {
	t.Parallel()
	service := Service{ID: "svc", Images: []string{"example/api:v1.0.0"}}
	state, checks := CompareServiceImages(service, []StatusResult{{
		ServiceID: "svc",
		Target:    "cluster",
		Health:    HealthHealthy,
		ObservedImages: []ObservedImage{
			NewObservedImage("cluster", "kubernetes", "example/api:v1.0.0", "sha256:v1", nil),
			NewObservedImage("cluster", "kubernetes", "example/api:v2.0.0", "sha256:v2", nil),
		},
	}})
	if state != ImageVersionMismatched {
		t.Fatalf("state = %s, want mismatched for mixed observed versions; checks=%#v", state, checks)
	}
	if len(checks) != 2 {
		t.Fatalf("checks = %#v, want matching desired check plus drift check", checks)
	}
	var matched, drifted bool
	for _, check := range checks {
		if check.Observed == nil {
			t.Fatalf("check = %#v, want observed image", check)
		}
		switch check.Observed.Reference.Tag {
		case "v1.0.0":
			matched = check.State == ImageVersionMatching
		case "v2.0.0":
			drifted = check.State == ImageVersionMismatched
		}
	}
	if !matched || !drifted {
		t.Fatalf("checks = %#v, want one matching v1 and one mismatched v2", checks)
	}
}
