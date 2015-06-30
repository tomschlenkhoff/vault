package vault

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/vault/helper/uuid"
	"github.com/hashicorp/vault/logical"
)

const (
	// coreMountConfigPath is used to store the mount configuration.
	// Mounts are protected within the Vault itself, which means they
	// can only be viewed or modified after an unseal.
	coreMountConfigPath = "core/mounts"

	// backendBarrierPrefix is the prefix to the UUID used in the
	// barrier view for the backends.
	backendBarrierPrefix = "logical/"

	// systemBarrierPrefix is sthe prefix used for the
	// system logical backend.
	systemBarrierPrefix = "sys/"
)

var (
	// loadMountsFailed if loadMounts encounters an error
	loadMountsFailed = errors.New("failed to setup mount table")

	// protectedMounts cannot be remounted
	protectedMounts = []string{
		"audit/",
		"auth/",
		"sys/",
	}
)

// MountTable is used to represent the internal mount table
type MountTable struct {
	// This lock should be held whenever modifying the Entries field.
	sync.RWMutex

	Entries []*MountEntry `json:"entries"`
}

// Returns a deep copy of the mount table
func (t *MountTable) Clone() *MountTable {
	mt := &MountTable{
		Entries: make([]*MountEntry, len(t.Entries)),
	}
	for i, e := range t.Entries {
		mt.Entries[i] = e.Clone()
	}
	return mt
}

// Hash is used to generate a hash value for the mount table
func (t *MountTable) Hash() ([]byte, error) {
	buf, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	hash := sha1.Sum(buf)
	return hash[:], nil
}

// Find is used to lookup an entry
func (t *MountTable) Find(path string) *MountEntry {
	n := len(t.Entries)
	for i := 0; i < n; i++ {
		if t.Entries[i].Path == path {
			return t.Entries[i]
		}
	}
	return nil
}

// SetTaint is used to set the taint on given entry
func (t *MountTable) SetTaint(path string, value bool) bool {
	n := len(t.Entries)
	for i := 0; i < n; i++ {
		if t.Entries[i].Path == path {
			t.Entries[i].Tainted = value
			return true
		}
	}
	return false
}

// Remove is used to remove a given path entry
func (t *MountTable) Remove(path string) bool {
	n := len(t.Entries)
	for i := 0; i < n; i++ {
		if t.Entries[i].Path == path {
			t.Entries[i], t.Entries[n-1] = t.Entries[n-1], nil
			t.Entries = t.Entries[:n-1]
			return true
		}
	}
	return false
}

// MountEntry is used to represent a mount table entry
type MountEntry struct {
	Path        string            `json:"path"`              // Mount Path
	Type        string            `json:"type"`              // Logical backend Type
	Description string            `json:"description"`       // User-provided description
	UUID        string            `json:"uuid"`              // Barrier view UUID
	Options     map[string]string `json:"options"`           // Backend configuration
	Tainted     bool              `json:"tainted,omitempty"` // Set as a Write-Ahead flag for unmount/remount
}

// Returns a deep copy of the mount entry
func (e *MountEntry) Clone() *MountEntry {
	optClone := make(map[string]string)
	for k, v := range e.Options {
		optClone[k] = v
	}
	return &MountEntry{
		Path:        e.Path,
		Type:        e.Type,
		Description: e.Description,
		UUID:        e.UUID,
		Options:     optClone,
	}
}

// Mount is used to mount a new backend to the mount table.
func (c *Core) mount(me *MountEntry) error {
	c.mounts.Lock()
	defer c.mounts.Unlock()

	// Ensure we end the path in a slash
	if !strings.HasSuffix(me.Path, "/") {
		me.Path += "/"
	}

	// Prevent protected paths from being unmounted
	for _, p := range protectedMounts {
		if strings.HasPrefix(me.Path, p) {
			return fmt.Errorf("cannot mount '%s'", me.Path)
		}
	}

	// Verify there is no conflicting mount
	if match := c.router.MatchingMount(me.Path); match != "" {
		return fmt.Errorf("existing mount at '%s'", match)
	}

	// Lookup the new backend
	backend, err := c.newLogicalBackend(me.Type, nil)
	if err != nil {
		return err
	}

	// Generate a new UUID and view
	me.UUID = uuid.GenerateUUID()
	view := NewBarrierView(c.barrier, backendBarrierPrefix+me.UUID+"/")

	// Update the mount table
	newTable := c.mounts.Clone()
	newTable.Entries = append(newTable.Entries, me)
	if err := c.persistMounts(newTable); err != nil {
		return errors.New("failed to update mount table")
	}
	c.mounts = newTable

	// Mount the backend
	if err := c.router.Mount(backend, me.Path, me.UUID, view); err != nil {
		return err
	}
	c.logger.Printf("[INFO] core: mounted '%s' type: %s", me.Path, me.Type)
	return nil
}

// Unmount is used to unmount a path.
func (c *Core) unmount(path string) error {
	c.mounts.Lock()
	defer c.mounts.Unlock()

	// Ensure we end the path in a slash
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}

	// Prevent protected paths from being unmounted
	for _, p := range protectedMounts {
		if strings.HasPrefix(path, p) {
			return fmt.Errorf("cannot unmount '%s'", path)
		}
	}

	// Verify exact match of the route
	match := c.router.MatchingMount(path)
	if match == "" || path != match {
		return fmt.Errorf("no matching mount")
	}

	// Store the view for this backend
	view := c.router.MatchingView(path)

	// Mark the entry as tainted
	if err := c.taintMountEntry(path); err != nil {
		return err
	}

	// Taint the router path to prevent routing
	if err := c.router.Taint(path); err != nil {
		return err
	}

	// Invoke the rollback manager a final time
	if err := c.rollback.Rollback(path); err != nil {
		return err
	}

	// Revoke all the dynamic keys
	if err := c.expiration.RevokePrefix(path); err != nil {
		return err
	}

	// Unmount the backend entirely
	if err := c.router.Unmount(path); err != nil {
		return err
	}

	// Clear the data in the view
	if err := ClearView(view); err != nil {
		return err
	}

	// Remove the mount table entry
	if err := c.removeMountEntry(path); err != nil {
		return err
	}
	c.logger.Printf("[INFO] core: unmounted '%s'", path)
	return nil
}

// removeMountEntry is used to remove an entry from the mount table
func (c *Core) removeMountEntry(path string) error {
	// Remove the entry from the mount table
	newTable := c.mounts.Clone()
	newTable.Remove(path)

	// Update the mount table
	if err := c.persistMounts(newTable); err != nil {
		return errors.New("failed to update mount table")
	}
	c.mounts = newTable
	return nil
}

// taintMountEntry is used to mark an entry in the mount table as tainted
func (c *Core) taintMountEntry(path string) error {
	// Remove the entry from the mount table
	newTable := c.mounts.Clone()
	newTable.SetTaint(path, true)

	// Update the mount table
	if err := c.persistMounts(newTable); err != nil {
		return errors.New("failed to update mount table")
	}
	c.mounts = newTable
	return nil
}

// Remount is used to remount a path at a new mount point.
func (c *Core) remount(src, dst string) error {
	c.mounts.Lock()
	defer c.mounts.Unlock()

	// Ensure we end the path in a slash
	if !strings.HasSuffix(src, "/") {
		src += "/"
	}
	if !strings.HasSuffix(dst, "/") {
		dst += "/"
	}

	// Prevent protected paths from being remounted
	for _, p := range protectedMounts {
		if strings.HasPrefix(src, p) {
			return fmt.Errorf("cannot remount '%s'", src)
		}
	}

	// Verify exact match of the route
	match := c.router.MatchingMount(src)
	if match == "" || src != match {
		return fmt.Errorf("no matching mount at '%s'", src)
	}

	// Verify there is no conflicting mount
	if match := c.router.MatchingMount(dst); match != "" {
		return fmt.Errorf("existing mount at '%s'", match)
	}

	// Mark the entry as tainted
	if err := c.taintMountEntry(src); err != nil {
		return err
	}

	// Taint the router path to prevent routing
	if err := c.router.Taint(src); err != nil {
		return err
	}

	// Invoke the rollback manager a final time
	if err := c.rollback.Rollback(src); err != nil {
		return err
	}

	// Revoke all the dynamic keys
	if err := c.expiration.RevokePrefix(src); err != nil {
		return err
	}

	// Update the entry in the mount table
	newTable := c.mounts.Clone()
	for _, ent := range newTable.Entries {
		if ent.Path == src {
			ent.Path = dst
			ent.Tainted = false
			break
		}
	}

	// Update the mount table
	if err := c.persistMounts(newTable); err != nil {
		return errors.New("failed to update mount table")
	}
	c.mounts = newTable

	// Remount the backend
	if err := c.router.Remount(src, dst); err != nil {
		return err
	}

	// Un-taint the path
	if err := c.router.Untaint(dst); err != nil {
		return err
	}

	c.logger.Printf("[INFO] core: remounted '%s' to '%s'", src, dst)
	return nil
}

// loadMounts is invoked as part of postUnseal to load the mount table
func (c *Core) loadMounts() error {
	// Load the existing mount table
	raw, err := c.barrier.Get(coreMountConfigPath)
	if err != nil {
		c.logger.Printf("[ERR] core: failed to read mount table: %v", err)
		return loadMountsFailed
	}
	if raw != nil {
		c.mounts = &MountTable{}
		if err := json.Unmarshal(raw.Value, c.mounts); err != nil {
			c.logger.Printf("[ERR] core: failed to decode mount table: %v", err)
			return loadMountsFailed
		}
	}

	// Done if we have restored the mount table
	if c.mounts != nil {
		return nil
	}

	// Create and persist the default mount table
	c.mounts = defaultMountTable()
	if err := c.persistMounts(c.mounts); err != nil {
		return loadMountsFailed
	}
	return nil
}

// persistMounts is used to persist the mount table after modification
func (c *Core) persistMounts(table *MountTable) error {
	// Marshal the table
	raw, err := json.Marshal(table)
	if err != nil {
		c.logger.Printf("[ERR] core: failed to encode mount table: %v", err)
		return err
	}

	// Create an entry
	entry := &Entry{
		Key:   coreMountConfigPath,
		Value: raw,
	}

	// Write to the physical backend
	if err := c.barrier.Put(entry); err != nil {
		c.logger.Printf("[ERR] core: failed to persist mount table: %v", err)
		return err
	}
	return nil
}

// setupMounts is invoked after we've loaded the mount table to
// initialize the logical backends and setup the router
func (c *Core) setupMounts() error {
	var backend logical.Backend
	var view *BarrierView
	var err error
	for _, entry := range c.mounts.Entries {
		// Initialize the backend, special casing for system
		barrierPath := backendBarrierPrefix + entry.UUID + "/"
		if entry.Type == "system" {
			barrierPath = systemBarrierPrefix
		}

		backend, err = c.newLogicalBackend(entry.Type, nil)
		if err != nil {
			c.logger.Printf(
				"[ERR] core: failed to create mount entry %#v: %v",
				entry, err)
			return loadMountsFailed
		}

		// Create a barrier view using the UUID
		view = NewBarrierView(c.barrier, barrierPath)

		if entry.Type == "system" {
			c.systemView = view
		}

		// Mount the backend
		err = c.router.Mount(backend, entry.Path, entry.UUID, view)
		if err != nil {
			c.logger.Printf("[ERR] core: failed to mount entry %#v: %v", entry, err)
			return loadMountsFailed
		}

		// Ensure the path is tainted if set in the mount table
		if entry.Tainted {
			c.router.Taint(entry.Path)
		}
	}
	return nil
}

// unloadMounts is used before we seal the vault to reset the mounts to
// their unloaded state. This is reversed by load and setup mounts.
func (c *Core) unloadMounts() error {
	c.mounts = nil
	c.router = NewRouter()
	c.systemView = nil
	return nil
}

// newLogicalBackend is used to create and configure a new logical backend by name
func (c *Core) newLogicalBackend(t string, conf map[string]string) (logical.Backend, error) {
	f, ok := c.logicalBackends[t]
	if !ok {
		return nil, fmt.Errorf("unknown backend type: %s", t)
	}

	b, err := f(conf)
	if err != nil {
		return nil, err
	}

	b.SetLogger(c.logger)
	return b, nil
}

// defaultMountTable creates a default mount table
func defaultMountTable() *MountTable {
	table := &MountTable{}
	genericMount := &MountEntry{
		Path:        "secret/",
		Type:        "generic",
		Description: "generic secret storage",
		UUID:        uuid.GenerateUUID(),
	}
	sysMount := &MountEntry{
		Path:        "sys/",
		Type:        "system",
		Description: "system endpoints used for control, policy and debugging",
		UUID:        uuid.GenerateUUID(),
	}
	table.Entries = append(table.Entries, genericMount)
	table.Entries = append(table.Entries, sysMount)
	return table
}
