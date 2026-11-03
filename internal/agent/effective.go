package agent

import (
	"fmt"

	"github.com/lemonity-org/azud/internal/config"
)

// Effective is what is running on this server. It is derived on each call from the
// server's configuration, State.Current and the record, so there is no third copy
// to keep in sync.
type Effective struct {
	// Config is what commands read: the current record's configuration with every
	// trust setting taken from the server's, and Image pinned to what the release
	// deploys. Before the first deploy it is the server's configuration as written.
	Config *config.Config
	// Deployed reports whether this server has a release.
	Deployed bool
	// Settling reports that a journal is open, so the server may be running parts
	// of two releases. Config is then the last whole release.
	Settling bool
	// Journal is the open journal, nil when not Settling.
	Journal *Journal
	// Record is the current release's record, nil when there is none: before the
	// first deploy, or an image deployed before the agent kept records.
	Record *Record
}

// LoadEffective reads what is running for server's service, from the state under
// stateDir. It takes no lock, because Store.Save renames atomically, so it works
// while the agent holds its locks, as inside a deploy_command script.
func LoadEffective(server *config.Config, stateDir string) (*Effective, error) {
	store := NewStore(stateDir, server.Service)
	st, err := store.Load()
	if err != nil {
		return nil, err
	}
	effective := &Effective{Config: server, Settling: st.Journal != nil, Journal: st.Journal}
	repository := config.RepositoryOf(server.Image)

	if st.Current != "" {
		rec, err := store.Records(repository).Load(st.Current)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			cfg, _, err := rec.Config(server)
			if err != nil {
				return nil, fmt.Errorf("stored release %s: %w", short(rec.Digest), err)
			}
			cfg.Image = rec.Image
			effective.Config, effective.Record, effective.Deployed = cfg, rec, true
			return effective, nil
		}
	}

	// An image v1 or a pushed deploy put there has no record yet: ImageSource adopts
	// it with the configuration on disk, so that is what it runs. A bundle's digest
	// is not an image, and a bundle with no record has nothing to read.
	if st.DeployedDigest != "" && !server.Agent.Bundles() {
		pinned := *server
		pinned.Image = repository + "@" + st.DeployedDigest
		effective.Config, effective.Deployed = &pinned, true
	}
	return effective, nil
}
