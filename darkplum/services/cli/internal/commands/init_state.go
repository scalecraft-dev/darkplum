package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// statePlan is where an S3 install keeps its Terraform state, decided before
// anything is written.
type statePlan struct {
	bucket, key string
	kept        bool // bucket and key come from an existing backend.tf
	st          backendState

	// Terraform's saved backend settings (.terraform/terraform.tfstate) naming
	// another bucket or key than the one init writes. Left alone, the next
	// `terraform init` stops with "Backend configuration changed".
	cache      *backendCache
	staleEmpty bool           // that earlier state is empty or gone: set the settings aside
	staleLive  *stateContents // that earlier state is not empty: never moved by init
}

func cacheExists(dir string) bool {
	_, err := os.Stat(backendCachePath(dir))
	return err == nil
}

// isFreshCheckout: no backend.tf and no saved backend settings, so nothing
// local says which state this checkout manages.
func isFreshCheckout(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "backend.tf"))
	return err != nil && !cacheExists(dir)
}

// preflightLiveInstall runs the live-install lookup for the requested cluster
// name before the name-collision check, so a fresh checkout of a live install
// is told to restore its backend.tf instead of being offered "<name>-2".
// Read-only.
func (c *InitCmd) preflightLiveInstall(env *initEnv, s *settings, dir string, cloud initCloud) error {
	if c.Backend == "local" || !isFreshCheckout(dir) {
		return nil
	}
	return c.checkLiveInstall(env, s, dir, cloud)
}

// checkLiveInstall looks for this cluster's live install in every Darkplum
// state bucket the account owns in the region. It fails closed: on a fresh
// checkout nothing local says which state is this install's, so a bucket list
// or a state object that cannot be read stops init rather than reading as
// "no install".
func (c *InitCmd) checkLiveInstall(env *initEnv, s *settings, dir string, cloud initCloud) error {
	fmt.Fprintf(env.out, "%s resolving Darkplum state buckets in %s…\n", sDim("…"), s.region)
	names, err := cloud.PrefixBuckets(s.region)
	if err != nil {
		return env.fail("existing install", "could not list state buckets: "+firstLine(err.Error()),
			"with no backend.tf here, init must check this account for an existing install first: grant s3:ListAllMyBuckets (and s3:ListBucket on "+
				stateBucketPrefix(s.account, s.region)+"*), or pass --backend local",
			"cannot check for an existing install")
	}
	return c.findLiveInstall(env, s, dir, cloud, names)
}

// findLiveInstall stops when any of the buckets already keeps non-empty state
// for this cluster (its own key, or the legacy key when the state there names
// this cluster or no cluster at all), and prints the backend.tf that lets this
// checkout manage that install. Any bucket with the Darkplum prefix counts,
// whatever its versioning, tags or public-access settings: those decide reuse
// for a new install, not whether an install exists.
func (c *InitCmd) findLiveInstall(env *initEnv, s *settings, dir string, cloud initCloud, buckets []string) error {
	w := env.out
	s.liveChecked = s.cluster
	keys := []string{legacyStateKey, stateKeyFor(s.cluster)}
	for _, name := range buckets {
		b := stateBucket{Name: name}
		for _, k := range keys {
			fmt.Fprintf(w, "%s resolving state at s3://%s/%s…\n", sDim("…"), b.Name, k)
			sc, found, err := cloud.ReadState(b.Name, k, s.region)
			if err != nil {
				return env.fail("existing install", "could not read s3://"+b.Name+"/"+k+": "+firstLine(err.Error()),
					"init must know whether an install already keeps state there and will not assume none: grant s3:GetObject on arn:aws:s3:::"+b.Name+"/"+k+
						" and kms:Decrypt on the bucket's KMS key ("+stateKeyAlias+"), then re-run",
					"cannot read existing state")
			}
			if !found || sc.empty() {
				continue
			}
			// The legacy key is shared by every older install: only one for
			// this cluster name (or one whose name cannot be read, so it might
			// be) stops init. Another named install is left alone.
			if k == legacyStateKey && sc.ClusterName != "" && sc.ClusterName != s.cluster {
				fmt.Fprintf(w, "  %s\n", sDim(fmt.Sprintf("another install (%s) keeps state at the earlier key in %s; left untouched", sc.ClusterName, b.Name)))
				continue
			}
			arn := kmsKeyPlaceholder
			if st, err := cloud.Inspect(b.Name, s.region); err == nil && st.KeyARN != "" {
				arn = st.KeyARN
			}
			block := renderBackend(backendSpec{Bucket: b.Name, Key: k, Region: s.region, KMSKeyARN: arn})
			who := "install " + s.cluster
			if sc.ClusterName == "" && k == legacyStateKey {
				who = "an install whose cluster name cannot be read (it may be " + s.cluster + ")"
			}
			hint := who + " already exists and keeps its state there: this is that install, not a name clash. To manage it from this checkout, save the following as backend.tf in " + dir +
				" and re-run init (it keeps an existing backend's bucket, key and region)"
			if k != legacyStateKey {
				hint += "; to install beside it instead, pass another --cluster-name"
			}
			printCheckTo(w, checkResult{name: "existing install", status: checkFail, detail: fmt.Sprintf("%s at s3://%s/%s", sc, b.Name, k), hint: hint})
			for _, l := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
				fmt.Fprintf(w, "      %s\n", l)
			}
			return reportedErr("an install already keeps state at s3://%s/%s", b.Name, k)
		}
	}
	return nil
}

// backendCache is the backend Terraform last initialised this directory with.
type backendCache struct {
	Type, Bucket, Key, Region string
}

func (b *backendCache) url() string { return "s3://" + b.Bucket + "/" + b.Key }

func backendCachePath(dir string) string {
	return filepath.Join(dir, ".terraform", "terraform.tfstate")
}

// readBackendCache returns nil (and no error) when the directory was never
// initialised or was initialised without a backend.
func readBackendCache(dir string) (*backendCache, error) {
	b, err := os.ReadFile(backendCachePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Backend *struct {
			Type   string `json:"type"`
			Config struct {
				Bucket string `json:"bucket"`
				Key    string `json:"key"`
				Region string `json:"region"`
			} `json:"config"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if f.Backend == nil || f.Backend.Type == "" {
		return nil, nil
	}
	return &backendCache{Type: f.Backend.Type, Bucket: f.Backend.Config.Bucket, Key: f.Backend.Config.Key, Region: f.Backend.Config.Region}, nil
}

// setAsideBackendCache renames the saved backend settings to
// terraform.tfstate.stale-<timestamp>, inside .terraform, so the next
// `terraform init` starts clean. Only called when the state they name is empty.
func setAsideBackendCache(dir string, now time.Time) error {
	p := backendCachePath(dir)
	return os.Rename(p, p+".stale-"+now.UTC().Format("20060102T150405Z"))
}

func migrateCmd(dir, profile string) string {
	cmd := "cd " + dir + " && terraform init -migrate-state"
	if profile != "" {
		arg := "-backend-config=profile=" + profile
		if strings.ContainsAny(arg, " \t\"'$`\\!*?[]{}();&|<>#~") {
			arg = shellQuote(arg)
		}
		cmd += " " + arg
	}
	return cmd
}

// planState picks the bucket and key, checks them, and looks at the saved
// backend settings. Read-only: it may ask questions, but creates, moves and
// writes nothing.
//
//   - An existing backend.tf for this account and region is kept exactly, bucket
//     and key: re-running init never moves an install's state.
//   - Otherwise a Darkplum state bucket already in the account and region is
//     reused (one: silently; several: the one the saved settings name, else
//     asked, else the newest under --yes), and only with none is one created.
//   - A new install's key carries the cluster name, and a key that already
//     holds resources belongs to another install: refused (or, interactively,
//     another cluster name is asked for).
func (c *InitCmd) planState(env *initEnv, s *settings, dir string, cloud initCloud) (statePlan, error) {
	w := env.out
	var p statePlan
	record := func(r checkResult) {
		printCheckTo(w, r)
		s.checks = append(s.checks, r)
	}

	cache, cacheErr := readBackendCache(dir)
	if cacheErr != nil {
		return p, env.fail("terraform backend", "saved settings unreadable: "+firstLine(cacheErr.Error()),
			"check "+filepath.Join(dir, ".terraform")+" (remove it if no deploy has run from this checkout), then re-run", "cannot read Terraform's saved backend settings")
	}
	if cache != nil && cache.Type != "s3" {
		cache = nil // local state is handled by the local-state check
	}

	backendPath := filepath.Join(dir, "backend.tf")
	_, statErr := os.Stat(backendPath)
	hasBackendTF := statErr == nil
	freshCheckout := !hasBackendTF && cache == nil && !cacheExists(dir)

	reused, note := false, ""
	if eb, ek, er, ok := existingBackend(backendPath); ok && er == s.region && strings.HasPrefix(eb, "darkplum-tfstate-"+s.account+"-") {
		p.bucket, p.key, p.kept, reused = eb, ek, true, true
		note = "same bucket, key and region"
	} else {
		// A checkout with no backend.tf and no saved settings may be a fresh
		// clone of a live install: find its state before choosing anything, so
		// a reinstall never quietly becomes a second install beside it.
		if freshCheckout && s.liveChecked != s.cluster {
			if err := c.checkLiveInstall(env, s, dir, cloud); err != nil {
				return p, err
			}
		}
		fmt.Fprintf(w, "%s resolving reusable state buckets in %s…\n", sDim("…"), s.region)
		buckets, err := cloud.StateBuckets(s.region)
		if err != nil {
			if freshCheckout {
				return p, env.fail("state buckets", "could not list: "+firstLine(err.Error()),
					"with no backend.tf here, init must see this account's state buckets: grant s3:ListAllMyBuckets (and read access to "+
						stateBucketPrefix(s.account, s.region)+"* and "+stateKeyAlias+"), or pass --backend local",
					"cannot list state buckets")
			}
			// Saved settings pin where this checkout's state is; a new bucket is safe.
			record(checkResult{name: "state buckets", status: checkWarn, detail: "could not list: " + firstLine(err.Error()),
				hint: "needs s3:ListAllMyBuckets to reuse an existing bucket; a new one will be created"})
		}
		var chosen stateBucket
		switch len(buckets) {
		case 0:
			p.bucket = stateBucketName(s.account, s.region, env.suffix())
		case 1:
			chosen, reused = buckets[0], true
		default:
			if chosen, err = pickStateBucket(env, buckets, cache); err != nil {
				return p, err
			}
			reused = true
		}
		if reused {
			p.bucket = chosen.Name
			if chosen.Untagged {
				note = "untagged, from an earlier release"
			}
		}
		p.key = stateKeyFor(s.cluster)
	}

	fmt.Fprintf(w, "%s checking state bucket %s and key %s…\n", sDim("…"), p.bucket, stateKeyAlias)
	st, err := cloud.Inspect(p.bucket, s.region)
	if err != nil {
		return p, env.fail("state bucket", firstLine(err.Error()), "fix the above, pick --backend local, or remove the bucket named in backend.tf", "cannot use the state backend")
	}
	p.st = st
	if p.kept && st.BucketExists && st.Versioning != "" && st.Versioning != "Enabled" {
		record(checkResult{name: "state versioning", status: checkWarn, detail: "versioning is " + st.Versioning + " on " + p.bucket,
			hint: "init turns it back on; state overwritten while it was off has no earlier versions"})
	}

	// A new install's key must not exist in a reused bucket: an object there,
	// even an empty one, is another install's (perhaps initialised, not yet
	// deployed). Only this checkout's own saved settings may claim it.
	for !p.kept && st.BucketExists {
		if cache != nil && cache.Bucket == p.bucket && cache.Key == p.key {
			break
		}
		fmt.Fprintf(w, "%s resolving state of %s in %s…\n", sDim("…"), s.cluster, p.bucket)
		sc, found, err := cloud.ReadState(p.bucket, p.key, s.region)
		if err != nil {
			return p, env.fail("state key", "could not read s3://"+p.bucket+"/"+p.key+": "+firstLine(err.Error()),
				"needs s3:GetObject on the bucket and kms:Decrypt on "+stateKeyAlias, "cannot read the state key")
		}
		if !found {
			break
		}
		msg := "another install named " + s.cluster + " already keeps state here"
		printCheckTo(w, checkResult{name: "state key", status: checkFail, detail: fmt.Sprintf("%s at s3://%s/%s", sc, p.bucket, p.key),
			hint: msg + "; choose another --cluster-name, or destroy that install first"})
		if !env.interactive {
			return p, reportedErr("%s", msg)
		}
		name, err := env.prompt.Ask("Cluster name", s.cluster+"-2", validateClusterName)
		if err != nil {
			return p, err
		}
		if s.cluster, err = chooseCluster(env, cloud, s.region, name, record); err != nil {
			return p, err
		}
		p.key = stateKeyFor(s.cluster)
		// The new name may itself be a live install's (legacy or cluster key).
		if freshCheckout {
			if err := c.checkLiveInstall(env, s, dir, cloud); err != nil {
				return p, err
			}
		}
	}

	detail := "S3 " + p.bucket
	if reused {
		if note != "" {
			detail += " (reused; " + note + ")"
		} else {
			detail += " (reused)"
		}
	}
	record(checkResult{name: "terraform state", status: checkPass, detail: detail})

	// Saved settings that name another location.
	if cache != nil && (cache.Bucket != p.bucket || cache.Key != p.key) {
		p.cache = cache
		region := cache.Region
		if region == "" {
			region = s.region
		}
		fmt.Fprintf(w, "%s resolving earlier state at %s…\n", sDim("…"), cache.url())
		sc, found, err := cloud.ReadState(cache.Bucket, cache.Key, region)
		switch {
		case err != nil:
			return p, env.fail("earlier state", "could not read "+cache.url()+": "+firstLine(err.Error()),
				"this checkout was last deployed with that state; check that the profile can read it (s3:GetObject, kms:Decrypt, same account) and re-run. Nothing was moved",
				"cannot read the earlier state")
		case !found || sc.empty():
			p.staleEmpty = true
		default:
			p.staleLive = &sc
		}
	}
	return p, nil
}

// pickStateBucket chooses among several reusable buckets: the one Terraform's
// saved settings name, else the operator's choice, else (--yes) the newest.
func pickStateBucket(env *initEnv, buckets []stateBucket, cache *backendCache) (stateBucket, error) {
	if cache != nil {
		for _, b := range buckets {
			if b.Name == cache.Bucket {
				return b, nil
			}
		}
	}
	if !env.interactive {
		fmt.Fprintf(env.out, "  %s\n", sDim(fmt.Sprintf("%d Darkplum state buckets in this account and region; using the newest, %s", len(buckets), buckets[0].Name)))
		return buckets[0], nil
	}
	opts := make([]Option, len(buckets))
	for i, b := range buckets {
		d := "created " + b.Created.UTC().Format("2006-01-02 15:04 MST")
		if b.Created.IsZero() {
			d = ""
		}
		opts[i] = Option{b.Name, d}
	}
	i, err := env.prompt.Select("Several Darkplum state buckets exist here. Which one should hold this install's state?", opts, 0)
	if err != nil {
		return stateBucket{}, err
	}
	return buckets[i], nil
}

// confirmStaleState: state with resources at the earlier location stays where
// it is. --yes refuses; an interactive run may write the new backend only after
// saying so, and the migrate step is printed again at the end.
func (c *InitCmd) confirmStaleState(env *initEnv, s *settings, dir string, p statePlan) error {
	cmd := migrateCmd(dir, s.profile)
	detail := fmt.Sprintf("%s at %s", p.staleLive, p.cache.url())
	if !env.interactive {
		return env.fail("earlier state", detail,
			"this checkout was last deployed with that state and init will not move it. Destroy that install first, or run init interactively and then move it: "+cmd,
			"earlier state still has resources; --yes will not switch the backend away from it")
	}
	printCheckTo(env.out, checkResult{name: "earlier state", status: checkWarn, detail: detail,
		hint: "it will not move by itself; after init run: " + cmd})
	ok, err := env.prompt.Confirm("Point Terraform at the new state location? The earlier state stays where it is until you migrate it", false)
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	return nil
}
