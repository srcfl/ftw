package state

import "time"

// DriverRepoInstall is durable activation metadata. Lua contents are stored in
// the repository manager's content-addressed directory, never in SQLite.
type DriverRepoInstall struct {
	ID                    int64  `json:"id"`
	RepoURL               string `json:"repo_url"`
	RepoID                string `json:"repo_id"`
	DriverID              string `json:"driver_id"`
	LogicalPath           string `json:"logical_path"`
	Version               string `json:"version"`
	SHA256                string `json:"sha256"`
	InstalledPath         string `json:"installed_path"`
	PreviousInstalledPath string `json:"previous_installed_path,omitempty"`
	InstalledAtMS         int64  `json:"installed_at_ms"`
	Active                bool   `json:"active"`
}

func (s *Store) ActivateDriverRepoInstall(in DriverRepoInstall) (DriverRepoInstall, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DriverRepoInstall{}, err
	}
	defer tx.Rollback()
	var previous string
	_ = tx.QueryRow(`SELECT installed_path FROM driver_repo_installs WHERE logical_path = ? AND active = 1`, in.LogicalPath).Scan(&previous)
	if _, err := tx.Exec(`UPDATE driver_repo_installs SET active = 0 WHERE logical_path = ?`, in.LogicalPath); err != nil {
		return DriverRepoInstall{}, err
	}
	in.PreviousInstalledPath = previous
	in.InstalledAtMS = time.Now().UnixMilli()
	if _, err := tx.Exec(`INSERT INTO driver_repo_installs
		(repo_url, repo_id, driver_id, logical_path, version, sha256, installed_path, previous_installed_path, installed_at_ms, active)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(repo_id, driver_id, version, sha256) DO UPDATE SET
			repo_url=excluded.repo_url, logical_path=excluded.logical_path,
			installed_path=excluded.installed_path,
			previous_installed_path=excluded.previous_installed_path,
			installed_at_ms=excluded.installed_at_ms, active=1`,
		in.RepoURL, in.RepoID, in.DriverID, in.LogicalPath, in.Version, in.SHA256,
		in.InstalledPath, in.PreviousInstalledPath, in.InstalledAtMS); err != nil {
		return DriverRepoInstall{}, err
	}
	if err := tx.Commit(); err != nil {
		return DriverRepoInstall{}, err
	}
	return s.ActiveDriverRepoInstall(in.LogicalPath)
}

func (s *Store) ActiveDriverRepoInstall(logicalPath string) (DriverRepoInstall, error) {
	return scanDriverRepoInstall(s.db.QueryRow(`SELECT id, repo_url, repo_id, driver_id, logical_path,
		version, sha256, installed_path, previous_installed_path, installed_at_ms, active
		FROM driver_repo_installs WHERE logical_path = ? AND active = 1`, logicalPath))
}

func (s *Store) DriverRepoInstallByPath(installedPath string) (DriverRepoInstall, error) {
	return scanDriverRepoInstall(s.db.QueryRow(`SELECT id, repo_url, repo_id, driver_id, logical_path,
		version, sha256, installed_path, previous_installed_path, installed_at_ms, active
		FROM driver_repo_installs WHERE installed_path = ? ORDER BY installed_at_ms DESC LIMIT 1`, installedPath))
}

func (s *Store) ActiveDriverRepoInstalls() ([]DriverRepoInstall, error) {
	rows, err := s.db.Query(`SELECT id, repo_url, repo_id, driver_id, logical_path,
		version, sha256, installed_path, previous_installed_path, installed_at_ms, active
		FROM driver_repo_installs WHERE active = 1 ORDER BY logical_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriverRepoInstall
	for rows.Next() {
		got, err := scanDriverRepoInstall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return out, rows.Err()
}

func (s *Store) DeactivateDriverRepoInstall(logicalPath string) error {
	_, err := s.db.Exec(`UPDATE driver_repo_installs SET active = 0 WHERE logical_path = ?`, logicalPath)
	return err
}

type driverRepoScanner interface{ Scan(...any) error }

func scanDriverRepoInstall(row driverRepoScanner) (DriverRepoInstall, error) {
	var out DriverRepoInstall
	var active int
	err := row.Scan(&out.ID, &out.RepoURL, &out.RepoID, &out.DriverID, &out.LogicalPath,
		&out.Version, &out.SHA256, &out.InstalledPath, &out.PreviousInstalledPath,
		&out.InstalledAtMS, &active)
	out.Active = active == 1
	return out, err
}
