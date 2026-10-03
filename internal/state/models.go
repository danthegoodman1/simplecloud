package state

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

// Overlay addressing. Each project gets a /24 from 10.88.0.0/16; the hub is .1
// and slots run from .10 upward.
const (
	OverlayBase     = "10.88"
	HubHostOctet    = 1
	FirstSlotOctet  = 10
	LastSlotOctet   = 250
	FirstListenPort = 51820
	LastListenPort  = 51899
	// LoopbackPrefix backs the per-service relays: a service name resolves to one
	// of these, so the agent knows which service and port a connection wanted.
	// The alias is derived from the overlay octet rather than allocated
	// separately, so the two can never disagree or exhaust at different points.
	// 127.0.1.x avoids 127.0.0.53, which a stub resolver may already hold.
	LoopbackPrefix = "127.0.1"
)

type Project struct {
	ID            string
	Name          string
	Dir           string
	OverlayCIDR   string
	OverlayIndex  int
	HubListenPort int
	HubPublicKey  string
	Region        string
}

// HubOverlayIP is the hub's address inside this project's network. The agent's
// heartbeat targets it, which is what restores the tunnel quickly after a resume.
func (p *Project) HubOverlayIP() string {
	return fmt.Sprintf("%s.%d.%d", OverlayBase, p.OverlayIndex, HubHostOctet)
}

func (p *Project) InterfaceName() string { return "wg-" + p.ID }

type Service struct {
	ProjectID   string
	Name        string
	Image       string
	ImageDigest string
	ImageID     string
	BuildHash   string
	ConfigHash  string
	Replicas    int
	Spec        string
}

type Slot struct {
	ProjectID       string
	Service         string
	Ordinal         int
	Name            string
	OverlayIP       string
	LoopbackIP      string
	WGPrivateKey    string
	WGPublicKey     string
	SandboxID       string
	SandboxStatus   string
	DoorbellPort    int
	DoorbellHost    string
	DoorbellToken   string
	DoorbellTokenID string
	DoorbellExpires int64
	AgentProcessID  string
	LogCursor       int64
	LastActiveAt    int64
}

type Volume struct {
	ProjectID  string
	Name       string
	DiskID     string
	MountToken string
	OwnerSlot  string
	MountPath  string
	Kind       string
}

type Exposure struct {
	ProjectID string
	Slot      string
	Port      int
	Hostname  string
}

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("state: generating id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// CreateProject binds a new project to dir and allocates its overlay range and
// hub listen port. Allocation picks the lowest free value so ranges stay dense.
func (s *Store) CreateProject(name, dir, region string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var p *Project
	err = s.Tx(func(tx *sql.Tx) error {
		idx, err := lowestFree(tx, `SELECT overlay_index FROM projects ORDER BY overlay_index`, 1, 254)
		if err != nil {
			return fmt.Errorf("allocating an overlay range: %w", err)
		}
		port, err := lowestFree(tx, `SELECT hub_listen_port FROM projects ORDER BY hub_listen_port`, FirstListenPort, LastListenPort)
		if err != nil {
			return fmt.Errorf("allocating a hub listen port: %w", err)
		}
		now := time.Now().Unix()
		p = &Project{
			ID:            newID(),
			Name:          name,
			Dir:           abs,
			OverlayCIDR:   fmt.Sprintf("%s.%d.0/24", OverlayBase, idx),
			OverlayIndex:  idx,
			HubListenPort: port,
			Region:        region,
		}
		_, err = tx.Exec(`INSERT INTO projects
			(id,name,dir,overlay_cidr,overlay_index,hub_listen_port,hub_public_key,region,created_at,updated_at)
			VALUES (?,?,?,?,?,?,'',?,?,?)`,
			p.ID, p.Name, p.Dir, p.OverlayCIDR, p.OverlayIndex, p.HubListenPort, p.Region, now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// lowestFree returns the smallest unused value in [lo, hi] from an ordered query.
func lowestFree(tx *sql.Tx, query string, lo, hi int) (int, error) {
	rows, err := tx.Query(query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	want := lo
	for rows.Next() {
		var got int
		if err := rows.Scan(&got); err != nil {
			return 0, err
		}
		if got > want {
			break
		}
		if got == want {
			want++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if want > hi {
		return 0, fmt.Errorf("no free value left between %d and %d", lo, hi)
	}
	return want, nil
}

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Name, &p.Dir, &p.OverlayCIDR, &p.OverlayIndex,
		&p.HubListenPort, &p.HubPublicKey, &p.Region)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

const projectCols = `id,name,dir,overlay_cidr,overlay_index,hub_listen_port,hub_public_key,region`

// ProjectForDir resolves the project bound to dir or any ancestor, which is what
// lets a command run from a subdirectory.
func (s *Store) ProjectForDir(dir string) (*Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE dir = ?`, abs))
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil, ErrNoProject
		}
		abs = parent
	}
}

func (s *Store) ProjectByName(name string) (*Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no project named %q", name)
	}
	return p, err
}

func (s *Store) ListProjects() ([]*Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RebindProject points an existing project at a new directory, for a project that
// has been moved.
func (s *Store) RebindProject(id, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE projects SET dir = ?, updated_at = ? WHERE id = ?`, abs, time.Now().Unix(), id)
	return err
}

func (s *Store) SetHubPublicKey(projectID, key string) error {
	_, err := s.db.Exec(`UPDATE projects SET hub_public_key = ?, updated_at = ? WHERE id = ?`,
		key, time.Now().Unix(), projectID)
	return err
}

func (s *Store) DeleteProject(id string) error {
	_, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

// ---- services ----

func (s *Store) PutService(svc *Service) error {
	_, err := s.db.Exec(`INSERT INTO services
		(project_id,name,image,image_digest,image_id,build_hash,config_hash,replicas,spec)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,name) DO UPDATE SET
			image=excluded.image, image_digest=excluded.image_digest,
			image_id=excluded.image_id, build_hash=excluded.build_hash,
			config_hash=excluded.config_hash, replicas=excluded.replicas, spec=excluded.spec`,
		svc.ProjectID, svc.Name, svc.Image, svc.ImageDigest, svc.ImageID,
		svc.BuildHash, svc.ConfigHash, svc.Replicas, svc.Spec)
	return err
}

func (s *Store) ListServices(projectID string) ([]*Service, error) {
	rows, err := s.db.Query(`SELECT project_id,name,image,image_digest,image_id,build_hash,config_hash,replicas,spec
		FROM services WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Service
	for rows.Next() {
		var v Service
		if err := rows.Scan(&v.ProjectID, &v.Name, &v.Image, &v.ImageDigest, &v.ImageID,
			&v.BuildHash, &v.ConfigHash, &v.Replicas, &v.Spec); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (s *Store) GetService(projectID, name string) (*Service, error) {
	var v Service
	err := s.db.QueryRow(`SELECT project_id,name,image,image_digest,image_id,build_hash,config_hash,replicas,spec
		FROM services WHERE project_id = ? AND name = ?`, projectID, name).
		Scan(&v.ProjectID, &v.Name, &v.Image, &v.ImageDigest, &v.ImageID, &v.BuildHash, &v.ConfigHash, &v.Replicas, &v.Spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &v, err
}

func (s *Store) DeleteService(projectID, name string) error {
	return s.Tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM slots WHERE project_id = ? AND service = ?`, projectID, name); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM services WHERE project_id = ? AND name = ?`, projectID, name)
		return err
	})
}

// ---- slots ----

const slotCols = `project_id,service,ordinal,name,overlay_ip,loopback_ip,wg_private_key,wg_public_key,
	sandbox_id,sandbox_status,doorbell_port,doorbell_host,doorbell_token,doorbell_token_id,
	doorbell_expires,agent_process_id,log_cursor,last_active_at`

func scanSlot(row interface{ Scan(...any) error }) (*Slot, error) {
	var v Slot
	err := row.Scan(&v.ProjectID, &v.Service, &v.Ordinal, &v.Name, &v.OverlayIP, &v.LoopbackIP,
		&v.WGPrivateKey, &v.WGPublicKey, &v.SandboxID, &v.SandboxStatus, &v.DoorbellPort,
		&v.DoorbellHost, &v.DoorbellToken, &v.DoorbellTokenID, &v.DoorbellExpires,
		&v.AgentProcessID, &v.LogCursor, &v.LastActiveAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) PutSlot(v *Slot) error {
	_, err := s.db.Exec(`INSERT INTO slots (`+slotCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,name) DO UPDATE SET
			ordinal=excluded.ordinal, overlay_ip=excluded.overlay_ip,
			loopback_ip=excluded.loopback_ip, sandbox_id=excluded.sandbox_id,
			sandbox_status=excluded.sandbox_status, doorbell_port=excluded.doorbell_port,
			doorbell_host=excluded.doorbell_host, doorbell_token=excluded.doorbell_token,
			doorbell_token_id=excluded.doorbell_token_id, doorbell_expires=excluded.doorbell_expires,
			agent_process_id=excluded.agent_process_id, log_cursor=excluded.log_cursor,
			last_active_at=excluded.last_active_at`,
		v.ProjectID, v.Service, v.Ordinal, v.Name, v.OverlayIP, v.LoopbackIP,
		v.WGPrivateKey, v.WGPublicKey, v.SandboxID, v.SandboxStatus, v.DoorbellPort,
		v.DoorbellHost, v.DoorbellToken, v.DoorbellTokenID, v.DoorbellExpires,
		v.AgentProcessID, v.LogCursor, v.LastActiveAt)
	return err
}

func (s *Store) ListSlots(projectID string) ([]*Slot, error) {
	rows, err := s.db.Query(`SELECT `+slotCols+` FROM slots WHERE project_id = ? ORDER BY service, ordinal`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Slot
	for rows.Next() {
		v, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetSlot(projectID, name string) (*Slot, error) {
	v, err := scanSlot(s.db.QueryRow(`SELECT `+slotCols+` FROM slots WHERE project_id = ? AND name = ?`, projectID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

func (s *Store) DeleteSlot(projectID, name string) error {
	_, err := s.db.Exec(`DELETE FROM slots WHERE project_id = ? AND name = ?`, projectID, name)
	return err
}

// AllocateSlot assigns the next free overlay and loopback addresses for a slot.
// A slot keeps both for life, so the hub's peer set does not change when a
// sandbox is replaced.
func (s *Store) AllocateSlot(p *Project, service string, ordinal int, privKey, pubKey string, doorbellPort int) (*Slot, error) {
	name := fmt.Sprintf("%s-%d", service, ordinal)
	if existing, err := s.GetSlot(p.ID, name); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}
	var slot *Slot
	err := s.Tx(func(tx *sql.Tx) error {
		host, err := lowestFreeOctet(tx, p.ID, `SELECT overlay_ip FROM slots WHERE project_id = ?`, FirstSlotOctet, LastSlotOctet)
		if err != nil {
			return fmt.Errorf("allocating an overlay address: %w", err)
		}
		slot = &Slot{
			ProjectID: p.ID, Service: service, Ordinal: ordinal, Name: name,
			OverlayIP:    fmt.Sprintf("%s.%d.%d", OverlayBase, p.OverlayIndex, host),
			LoopbackIP:   fmt.Sprintf("%s.%d", LoopbackPrefix, host),
			WGPrivateKey: privKey, WGPublicKey: pubKey, DoorbellPort: doorbellPort,
		}
		_, err = tx.Exec(`INSERT INTO slots (`+slotCols+`) VALUES (?,?,?,?,?,?,?,?,'','',?,'','','',0,'',0,0)`,
			slot.ProjectID, slot.Service, slot.Ordinal, slot.Name, slot.OverlayIP, slot.LoopbackIP,
			slot.WGPrivateKey, slot.WGPublicKey, slot.DoorbellPort)
		return err
	})
	if err != nil {
		return nil, err
	}
	return slot, nil
}

// lowestFreeOctet finds the smallest unused final octet among existing addresses.
func lowestFreeOctet(tx *sql.Tx, projectID, query string, lo, hi int) (int, error) {
	rows, err := tx.Query(query, projectID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return 0, err
		}
		if ip := net.ParseIP(addr); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				used[int(v4[3])] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i := lo; i <= hi; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free address left between %d and %d", lo, hi)
}

// ---- volumes and exposures ----

func (s *Store) PutVolume(v *Volume) error {
	_, err := s.db.Exec(`INSERT INTO volumes (project_id,name,disk_id,mount_token,owner_slot,mount_path,kind)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(project_id,name) DO UPDATE SET
			disk_id=excluded.disk_id, mount_token=excluded.mount_token,
			owner_slot=excluded.owner_slot, mount_path=excluded.mount_path, kind=excluded.kind`,
		v.ProjectID, v.Name, v.DiskID, v.MountToken, v.OwnerSlot, v.MountPath, v.Kind)
	return err
}

func (s *Store) ListVolumes(projectID string) ([]*Volume, error) {
	rows, err := s.db.Query(`SELECT project_id,name,disk_id,mount_token,owner_slot,mount_path,kind
		FROM volumes WHERE project_id = ? ORDER BY name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.ProjectID, &v.Name, &v.DiskID, &v.MountToken, &v.OwnerSlot, &v.MountPath, &v.Kind); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (s *Store) DeleteVolume(projectID, name string) error {
	_, err := s.db.Exec(`DELETE FROM volumes WHERE project_id = ? AND name = ?`, projectID, name)
	return err
}

func (s *Store) PutExposure(e *Exposure) error {
	_, err := s.db.Exec(`INSERT INTO exposures (project_id,slot,port,hostname) VALUES (?,?,?,?)
		ON CONFLICT(project_id,slot,port) DO UPDATE SET hostname = excluded.hostname`,
		e.ProjectID, e.Slot, e.Port, e.Hostname)
	return err
}

func (s *Store) ListExposures(projectID string) ([]*Exposure, error) {
	rows, err := s.db.Query(`SELECT project_id,slot,port,hostname FROM exposures WHERE project_id = ? ORDER BY slot, port`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Exposure
	for rows.Next() {
		var e Exposure
		if err := rows.Scan(&e.ProjectID, &e.Slot, &e.Port, &e.Hostname); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteExposure(projectID, slot string, port int) error {
	_, err := s.db.Exec(`DELETE FROM exposures WHERE project_id = ? AND slot = ? AND port = ?`, projectID, slot, port)
	return err
}

// ---- hub ----

type Hub struct {
	SSHTarget    string
	Endpoint     string
	HostKey      string
	Bootstrapped bool
}

func (s *Store) GetHub() (*Hub, error) {
	var h Hub
	var boot int
	err := s.db.QueryRow(`SELECT ssh_target,endpoint,host_key,bootstrapped FROM hub WHERE id = 1`).
		Scan(&h.SSHTarget, &h.Endpoint, &h.HostKey, &boot)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	h.Bootstrapped = boot == 1
	return &h, err
}

func (s *Store) PutHub(h *Hub) error {
	boot := 0
	if h.Bootstrapped {
		boot = 1
	}
	_, err := s.db.Exec(`INSERT INTO hub (id,ssh_target,endpoint,host_key,bootstrapped,updated_at)
		VALUES (1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET ssh_target=excluded.ssh_target, endpoint=excluded.endpoint,
			host_key=excluded.host_key, bootstrapped=excluded.bootstrapped, updated_at=excluded.updated_at`,
		h.SSHTarget, h.Endpoint, h.HostKey, boot, time.Now().Unix())
	return err
}
