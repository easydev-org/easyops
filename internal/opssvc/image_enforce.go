package opssvc

import (
	"strings"

	"connectrpc.com/connect/v2"
)

// splitImageRef splits an image reference into its registry host and the
// remainder (repository path + optional tag/digest). The first path segment is a
// registry host only when it contains '.' or ':' or equals "localhost" — a bare
// `redis:7` is a tag, not a host — mirroring imagebuild.registryHostOf. When the
// ref carries no explicit host, host is "" and the whole ref is the remainder.
func splitImageRef(ref string) (host, remainder string) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "://"); i >= 0 {
		ref = ref[i+3:]
	}
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return "", ref
	}
	first := ref[:slash]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first, ref[slash+1:]
	}
	return "", ref
}

// repoPath strips an image's tag/digest, leaving just the repository path.
func repoPath(remainder string) string {
	if i := strings.LastIndex(remainder, "@"); i >= 0 {
		remainder = remainder[:i]
	}
	// Drop a tag only when it is in the LAST path segment (not an org:port).
	if i := strings.LastIndex(remainder, ":"); i >= 0 && !strings.Contains(remainder[i:], "/") {
		remainder = remainder[:i]
	}
	return remainder
}

// orgOf returns the first path segment (the org / namespace) of a repository
// path.
func orgOf(remainder string) string {
	return strings.SplitN(repoPath(remainder), "/", 2)[0]
}

// enforceSandboxImage validates (and, for a bare `<org>/<name>:<tag>` ref,
// rewrites to include the registry host) a sandbox image so it can only come
// from the deployment's artifact registry (`IMAGE_REGISTRY_HOST`) AND the
// dedicated sandbox org (`SANDBOX_ORG`). It returns the (possibly rewritten)
// image reference.
//
// When IMAGE_REGISTRY_HOST is unset the guard is disabled and the image is
// accepted unchanged (the deployment opted out) — the pre-enforcement behaviour.
func (s *Service) enforceSandboxImage(image string) (string, error) {
	return s.enforceImage(image, s.sandboxOrg, "sandbox image")
}

// enforceServiceImage is the service-side analogue: a long-lived service image
// must come from artifact (`IMAGE_REGISTRY_HOST`), but any org is allowed (a
// service runs the caller's own built image).
func (s *Service) enforceServiceImage(image string) (string, error) {
	return s.enforceImage(image, "", "service image")
}

// enforceImage is the shared implementation. `requiredOrg` (when non-empty)
// additionally pins the image to that org.
func (s *Service) enforceImage(image, requiredOrg, what string) (string, error) {
	host := strings.TrimSpace(s.imageRegistryHost)
	if host == "" {
		// Guard disabled: the deployment did not configure a registry host.
		return image, nil
	}
	gotHost, remainder := splitImageRef(image)
	if gotHost == "" {
		// No explicit host: a bare `<org>/<name>:<tag>`. If it already names the
		// right org we can safely rewrite it to the configured registry; a bare
		// single-segment name has no org and is rejected below.
		gotHost = host
		image = host + "/" + remainder
	}
	if gotHost != host {
		return "", connect.NewError(connect.CodeInvalidArgument,
			what+" must come from the artifact registry "+host+", not "+gotHost)
	}
	if requiredOrg != "" && orgOf(remainder) != requiredOrg {
		return "", connect.NewError(connect.CodeInvalidArgument,
			what+" must come from the "+requiredOrg+" org")
	}
	return image, nil
}
