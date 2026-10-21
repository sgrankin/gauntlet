package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func (b *Broker) completed(ctx context.Context, id string) (Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var data []byte
	if err := b.db.QueryRowContext(ctx, `SELECT result FROM worker_jobs WHERE id=? AND state='completed'`, id).Scan(&data); err != nil {
		return Result{}, err
	}
	var result Result
	err := json.Unmarshal(data, &result)
	return result, err
}

func (s *Server) combineLogs(ctx context.Context, parent string, current string) (string, error) {
	sources := []string{}
	if parent != "" {
		previous, err := s.Broker.completed(ctx, parent)
		if err != nil {
			return "", err
		}
		if previous.Check.LogPath != "" {
			sources = append(sources, previous.Check.LogPath)
		}
	}
	if current == "" {
		return "", nil
	}
	sources = append(sources, current)
	file, err := os.CreateTemp(filepath.Dir(current), ".combined-")
	if err != nil {
		return "", err
	}
	defer file.Close()
	defer os.Remove(file.Name())
	for _, path := range sources {
		source, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(file, source)
		source.Close()
		if err != nil {
			return "", err
		}
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	combined := current + ".diagnostic.log.zst"
	if err = os.Rename(file.Name(), combined); err != nil {
		return "", err
	}
	return combined, nil
}

func logNote(result *Result, err error) {
	result.Check.Output += "\ngauntlet: worker diagnostic log could not be assembled: " + fmt.Sprint(err)
	if len(result.Check.Output) > 65536 {
		result.Check.Output = result.Check.Output[len(result.Check.Output)-65536:]
	}
}
