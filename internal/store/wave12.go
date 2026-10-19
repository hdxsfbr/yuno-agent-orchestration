package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Wave 12 (DESIGN 5.27, migration 0016): the operator's edits to an agent's workspace files
// and the files of a skill bundle.

// GetAgentFileEdits returns the agent's stored edits by file name; a file with no edit is
// absent. The map is never nil.
func GetAgentFileEdits(ctx context.Context, q Querier, agentID string) (map[string]AgentFileEdit, error) {
	rows, err := q.Query(ctx, `SELECT agent_id, name, addition, override, updated_at
		FROM agent_file_edits WHERE agent_id=$1 ORDER BY name`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AgentFileEdit{}
	for rows.Next() {
		var e AgentFileEdit
		if err := rows.Scan(&e.AgentID, &e.Name, &e.Addition, &e.Override, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out[e.Name] = e
	}
	return out, rows.Err()
}

// UpsertAgentFileAddition stores the addition of a generated file; an empty addition clears
// it. The row is deleted when neither an addition nor an override remains.
func UpsertAgentFileAddition(ctx context.Context, q Querier, agentID, name, addition string) error {
	if addition == "" {
		return clearAgentFileEdit(ctx, q, agentID, name, "addition")
	}
	_, err := q.Exec(ctx, `INSERT INTO agent_file_edits (agent_id, name, addition, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (agent_id, name) DO UPDATE SET addition=EXCLUDED.addition, updated_at=now()`,
		agentID, name, addition)
	return err
}

// UpsertAgentFileOverride stores the override of a runtime file; nil deletes it (an empty
// non-nil override is kept: the operator emptied the file). The row is deleted when neither
// an addition nor an override remains.
func UpsertAgentFileOverride(ctx context.Context, q Querier, agentID, name string, override *string) error {
	if override == nil {
		return clearAgentFileEdit(ctx, q, agentID, name, "override")
	}
	_, err := q.Exec(ctx, `INSERT INTO agent_file_edits (agent_id, name, override, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (agent_id, name) DO UPDATE SET override=EXCLUDED.override, updated_at=now()`,
		agentID, name, *override)
	return err
}

// clearAgentFileEdit nulls one column of the row and drops the row once both are empty.
func clearAgentFileEdit(ctx context.Context, q Querier, agentID, name, column string) error {
	if column != "addition" && column != "override" {
		return errors.New("store: unknown agent file column")
	}
	if _, err := q.Exec(ctx, `UPDATE agent_file_edits SET `+column+`=NULL, updated_at=now()
		WHERE agent_id=$1 AND name=$2`, agentID, name); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `DELETE FROM agent_file_edits
		WHERE agent_id=$1 AND name=$2 AND (addition IS NULL OR addition='') AND override IS NULL`, agentID, name)
	return err
}

// SkillManifestFile is the file the platform writes in an assigned skill's workspace folder
// listing the bundle paths it wrote (one per line). It is not a bundle file.
const SkillManifestFile = ".platform-files"

// ListSkillFiles returns a skill's bundle files with their content, by path.
func ListSkillFiles(ctx context.Context, q Querier, skillID string) ([]SkillFile, error) {
	return listSkillFiles(ctx, q, skillID, true)
}

// ListSkillFileMeta returns a skill's bundle files without content (Content is nil), by path.
func ListSkillFileMeta(ctx context.Context, q Querier, skillID string) ([]SkillFile, error) {
	return listSkillFiles(ctx, q, skillID, false)
}

func listSkillFiles(ctx context.Context, q Querier, skillID string, content bool) ([]SkillFile, error) {
	col := "NULL::bytea"
	if content {
		col = "content"
	}
	rows, err := q.Query(ctx, `SELECT path, `+col+`, mode, size, sha256 FROM skill_files WHERE skill_id=$1 ORDER BY path`, skillID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SkillFile{}
	for rows.Next() {
		var f SkillFile
		if err := rows.Scan(&f.Path, &f.Content, &f.Mode, &f.Size, &f.SHA256); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListSkillFileDigests returns the bundle files (path, mode, size, SHA256; no content) of all
// the given skills in one query, by skill id and then path. A skill with no files is absent.
func ListSkillFileDigests(ctx context.Context, q Querier, skillIDs []string) (map[string][]SkillFile, error) {
	return listSkillFilesOf(ctx, q, skillIDs, false)
}

// ListSkillFilesContent is ListSkillFileDigests with the content, for the write path.
func ListSkillFilesContent(ctx context.Context, q Querier, skillIDs []string) (map[string][]SkillFile, error) {
	return listSkillFilesOf(ctx, q, skillIDs, true)
}

func listSkillFilesOf(ctx context.Context, q Querier, skillIDs []string, content bool) (map[string][]SkillFile, error) {
	out := map[string][]SkillFile{}
	if len(skillIDs) == 0 {
		return out, nil
	}
	col := "NULL::bytea"
	if content {
		col = "content"
	}
	rows, err := q.Query(ctx, `SELECT skill_id, path, `+col+`, mode, size, sha256 FROM skill_files
		WHERE skill_id = ANY($1) ORDER BY skill_id, path`, skillIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f SkillFile
		if err := rows.Scan(&id, &f.Path, &f.Content, &f.Mode, &f.Size, &f.SHA256); err != nil {
			return nil, err
		}
		out[id] = append(out[id], f)
	}
	return out, rows.Err()
}

// GetSkillFile returns one bundle file with its content, or ErrNotFound.
func GetSkillFile(ctx context.Context, q Querier, skillID, path string) (*SkillFile, error) {
	var f SkillFile
	err := q.QueryRow(ctx, `SELECT path, content, mode, size, sha256 FROM skill_files WHERE skill_id=$1 AND path=$2`,
		skillID, path).Scan(&f.Path, &f.Content, &f.Mode, &f.Size, &f.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// PutSkillFile inserts or replaces one bundle file; Size is set from the content. It also
// moves the skill's updated_at, and is ErrNotFound for an unknown skill.
func PutSkillFile(ctx context.Context, q Querier, skillID string, f *SkillFile) error {
	if f.Content == nil {
		f.Content = []byte{}
	}
	f.Size = len(f.Content)
	sum := sha256.Sum256(f.Content)
	f.SHA256 = sum[:]
	if _, err := GetSkill(ctx, q, skillID); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `INSERT INTO skill_files (skill_id, path, content, mode, size, sha256) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (skill_id, path) DO UPDATE SET content=EXCLUDED.content, mode=EXCLUDED.mode, size=EXCLUDED.size, sha256=EXCLUDED.sha256`,
		skillID, f.Path, f.Content, f.Mode, f.Size, f.SHA256); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE skills SET updated_at=$2 WHERE id=$1`, skillID, time.Now().UTC())
	return err
}

// DeleteSkillFile removes one bundle file, or is ErrNotFound.
func DeleteSkillFile(ctx context.Context, q Querier, skillID, path string) error {
	tag, err := q.Exec(ctx, `DELETE FROM skill_files WHERE skill_id=$1 AND path=$2`, skillID, path)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = q.Exec(ctx, `UPDATE skills SET updated_at=$2 WHERE id=$1`, skillID, time.Now().UTC())
	return err
}

// BatchQuerier is a pgx.Tx or a pool: anything that sends a batch.
type BatchQuerier interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// ReplaceSkillFiles makes the skill's bundle exactly files: it deletes the rows and inserts
// the new ones in one batch. Call it with a transaction so the replacement is atomic (an
// import with ?replace=true, DESIGN 5.27 rule 6); on a bare pool the batch still runs as one
// implicit transaction.
func ReplaceSkillFiles(ctx context.Context, q BatchQuerier, skillID string, files []SkillFile) error {
	b := &pgx.Batch{}
	b.Queue(`DELETE FROM skill_files WHERE skill_id=$1`, skillID)
	for _, f := range files {
		content := f.Content
		if content == nil {
			content = []byte{}
		}
		sum := sha256.Sum256(content)
		b.Queue(`INSERT INTO skill_files (skill_id, path, content, mode, size, sha256) VALUES ($1,$2,$3,$4,$5,$6)`,
			skillID, f.Path, content, f.Mode, len(content), sum[:])
	}
	b.Queue(`UPDATE skills SET updated_at=now() WHERE id=$1`, skillID)
	br := q.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}
