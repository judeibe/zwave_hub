package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// DiscoveryOptions configures the sysfs poller. Every value is injected: the
// package reads neither the environment nor a fixed /sys or /dev (constitution
// III), which is also what lets the tests run against the fixture tree.
type DiscoveryOptions struct {
	// SysRoot is the sysfs root, "/sys" in production.
	SysRoot string
	// DevRoot is the dev root the nodes are opened through, "/dev" in
	// production. It is where the by-id link and the permission check live.
	DevRoot string
	// Interval is the poll period. config clamps it to at least
	// MinDiscoveryInterval.
	Interval time.Duration
	// Debounce is how long a changed device set must stay unchanged before it
	// is published (R14 §7). Zero publishes on the poll that saw the change.
	Debounce time.Duration
	// Publish receives each settled device set. The first poll publishes
	// immediately; later sets publish only after the debounce. An error leaves
	// the set unpublished, so the next poll retries it.
	Publish func(context.Context, []Device) error
}

// DefaultDebounce is how long a changed device set must stay unchanged before it
// is published (research.md R14 §7). The deployment uses it; a test may set
// its own.
const DefaultDebounce = 300 * time.Millisecond

// Discovery polls sysfs and publishes the attached USB serial devices it maps
// to a /dev node. It owns no durable state: identity, support and the selection
// live in the hub and the store, and this type only reports what the host
// currently shows.
type Discovery struct {
	sysRoot  string
	devRoot  string
	interval time.Duration
	settle   settleState
	publish  func(context.Context, []Device) error

	// access is unix.Access with R_OK|W_OK. It is a field so a test can force
	// the permission verdict without running as a particular user.
	access func(path string) error
}

// NewDiscovery validates opts and returns a poller. Run may be called once.
func NewDiscovery(opts DiscoveryOptions) (*Discovery, error) {
	switch {
	case opts.SysRoot == "":
		return nil, errors.New("coordinator: discovery sysfs root is empty")
	case opts.DevRoot == "":
		return nil, errors.New("coordinator: discovery dev root is empty")
	case opts.Interval <= 0:
		return nil, errors.New("coordinator: discovery interval must be positive")
	case opts.Debounce < 0:
		return nil, errors.New("coordinator: discovery debounce must not be negative")
	case opts.Publish == nil:
		return nil, errors.New("coordinator: discovery publish function is nil")
	}
	return &Discovery{
		sysRoot:  opts.SysRoot,
		devRoot:  opts.DevRoot,
		interval: opts.Interval,
		settle:   settleState{debounce: opts.Debounce},
		publish:  opts.Publish,
		access: func(path string) error {
			return unix.Access(path, unix.R_OK|unix.W_OK)
		},
	}, nil
}

// Run polls until ctx is cancelled. The first poll runs immediately, so a hub
// that starts with a stick already plugged in reports it without waiting one
// interval.
//
// A poll that cannot read sysfs publishes nothing: the last good device set
// stands. Wiping the list would report "nothing attached" when the truth is
// "could not look", which is the worse lie; contracts/events.md has no event
// for a sysfs read failure, so there is nothing honest to emit either.
func (d *Discovery) Run(ctx context.Context) {
	d.pollAndPublish(ctx, time.Now())

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			d.pollAndPublish(ctx, now)
		}
	}
}

// pollAndPublish performs one poll and publishes the set when it has settled.
func (d *Discovery) pollAndPublish(ctx context.Context, now time.Time) {
	devices, err := d.poll()
	if err != nil {
		return
	}
	snapshot, ok := d.settle.observe(now, devices)
	if !ok {
		return
	}
	if err := d.publish(ctx, snapshot); err != nil {
		return
	}
	d.settle.commit(snapshot)
}

// poll reads sysfs, maps every device to its /dev node, and resolves identity
// and ambiguity over the mapped set.
func (d *Discovery) poll() ([]Device, error) {
	devices, err := Enumerate(d.sysRoot)
	if err != nil {
		return nil, err
	}
	for i := range devices {
		d.crossCheck(&devices[i])
	}
	return ResolveAll(devices), nil
}

// crossCheck decides whether a sysfs device is reachable from the container and
// at which path. The by-id link wins when it resolves to the node, because it
// is the path the driver should open (R14 §4); the raw tty node is the fallback.
// A device with neither is not passed through, and one the hub cannot read and
// write is permission denied — two faults with different remediations, which is
// why they are separate reasons (R5, FR-006).
func (d *Discovery) crossCheck(dev *Device) {
	if link, ok := d.byIDLink(dev.TTY); ok {
		dev.Attached = true
		dev.Path = link
		dev.PathKind = PathByID
	} else if node := filepath.Join(d.devRoot, dev.TTY); exists(node) {
		dev.Attached = true
		dev.Path = node
		dev.PathKind = PathTTY
	} else {
		dev.Reason = ReasonNotPassedThrough
		return
	}

	dev.Accessible = d.access(dev.Path) == nil
	if !dev.Accessible {
		dev.Reason = ReasonPermissionDenied
	}
}

// byIDLink returns the /dev/serial/by-id link that points at the tty, if one
// exists. Entries are read in name order, so a tty with more than one link
// always resolves to the same one.
func (d *Discovery) byIDLink(tty string) (string, bool) {
	dir := filepath.Join(d.devRoot, "serial", "by-id")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	node := filepath.Join(d.devRoot, tty)
	resolvedNode, nodeErr := filepath.EvalSymlinks(node)
	for _, entry := range entries {
		link := filepath.Join(dir, entry.Name())
		target, err := filepath.EvalSymlinks(link)
		if err != nil {
			continue
		}
		if nodeErr == nil {
			if target == resolvedNode {
				return link, true
			}
			continue
		}
		// The raw node does not resolve, but a link whose target is still a
		// tty with this name is a usable path to it.
		if filepath.Base(target) == tty {
			return link, true
		}
	}
	return "", false
}

// exists reports whether path resolves to something. A dangling symlink is not
// a node the driver can open.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// settleState turns a stream of polls into published sets. It exists so the
// debounce rule of R14 §7 is a pure function of time and the polled set, which
// a test can drive with synthetic timestamps instead of sleeping.
type settleState struct {
	debounce time.Duration

	published     string
	havePublished bool

	candidate      []Device
	candidateSig   string
	candidateSince time.Time
}

// observe records one poll at now and returns the set to publish. The first
// successful poll publishes at once; after that a set publishes only when it
// has been unchanged for the debounce window, so a pull-and-replug that settles
// within 300 ms produces one event, not a flap.
func (s *settleState) observe(now time.Time, devices []Device) ([]Device, bool) {
	sig := deviceSignature(devices)
	if s.havePublished && sig == s.published {
		s.candidate = nil
		s.candidateSig = ""
		return nil, false
	}
	if s.candidate == nil || sig != s.candidateSig {
		s.candidate = devices
		s.candidateSig = sig
		s.candidateSince = now
		if !s.havePublished {
			return devices, true
		}
	}
	if now.Sub(s.candidateSince) < s.debounce {
		return nil, false
	}
	return s.candidate, true
}

// commit records that the set returned by observe was published. It is separate
// from observe so a failed publish does not advance the state and the next poll
// retries the same set.
func (s *settleState) commit(devices []Device) {
	s.published = deviceSignature(devices)
	s.havePublished = true
	s.candidate = nil
	s.candidateSig = ""
}

// deviceSignature is a stable description of everything a reader can see in a
// device set. Any change to identity, path, reachability or verdict changes it;
// nothing else does.
func deviceSignature(devices []Device) string {
	var b strings.Builder
	for _, d := range devices {
		fmt.Fprintf(&b, "%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%t\x1f%t\x1f%t\x1f%s\x1f%s\n",
			d.TTY, d.Identity.ID, d.Identity.Kind, d.Path, d.PathKind,
			d.Attributes.Topology, d.Attached, d.Accessible, d.Ambiguous,
			d.Support, d.Reason)
	}
	return b.String()
}
