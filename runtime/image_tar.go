package main

// imageTarConfig is a docker image delivered as a TARBALL fetched by Nomad,
// loaded by the docker driver, instead of pulled from a registry.
//
// WHY IT EXISTS. Prospective adopters hit the same wall from two directions:
// GC_NOMAD_AGENT_IMAGE is required with no default and producing it is a
// multi-stage manual process, and a bare local tag "only works via a node's
// local cache, silently pinning every session to whichever node built it".
// This pack already DETECTS the second (registrylessImageWarning: a bare name
// "names no registry, so no node can ever pull it") — but detecting it just
// tells the adopter to go stand up a registry, which is infrastructure the
// pack does not ship and must not assume.
//
// Nomad's docker driver answers this itself: `load = "<tarball>"`, documented
// as equivalent to `docker load -i`, fed by an ordinary artifact stanza. No
// registry, and — unlike running a registry in-cluster — no per-client daemon
// configuration either, which matters because this pack deliberately keeps its
// cluster ask small enough to evaluate in an hour (see
// no_cluster_prereqs_test.go).
//
// The artifact MUST declare archive=false. Nomad auto-extracts a recognised
// archive, and an extracted tar is not a tar the driver can load.
type imageTarConfig struct {
	// Source is the tarball URL, or "" for "no tarball; pull the image".
	Source string
	// Checksum is whatever Nomad's getter accepts, including the
	// "file:<url>.sha256" sidecar form the gc artifact already uses.
	Checksum string
}

func (c imageTarConfig) enabled() bool { return c.Source != "" }

// imageTarDest is the task-dir subdirectory the tarball lands in.
const imageTarDest = "imagetar"

// sessionTaskOpt lets sessionTask and its callers take EITHER optional shape
// without a second variadic (Go allows one) and without touching the three
// dozen existing call sites that pass a gcArtifactConfig positionally.
type sessionTaskOpt interface{ isSessionTaskOpt() }

func (gcArtifactConfig) isSessionTaskOpt() {}
func (imageTarConfig) isSessionTaskOpt()   {}

// splitSessionTaskOpts picks the last of each shape, mirroring firstArtifact's
// collapse-to-one-value contract.
func splitSessionTaskOpts(opts []sessionTaskOpt) (gcArtifactConfig, imageTarConfig) {
	var art gcArtifactConfig
	var tar imageTarConfig
	for _, o := range opts {
		switch v := o.(type) {
		case gcArtifactConfig:
			art = v
		case imageTarConfig:
			tar = v
		}
	}
	return art, tar
}
