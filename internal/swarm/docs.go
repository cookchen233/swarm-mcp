package swarm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DocsService struct {
	store *Store
}

func NewDocsService(store *Store) *DocsService {
	return &DocsService{store: store}
}

// Docs are stored under:
// - docs/shared/{name}.md
// - issues/{issue_id}/docs/{name}.md
// - issues/{issue_id}/tasks/{task_id}.docs/{name}.md
//
// Note: name can include subdirectories; it will be cleaned.

func (d *DocsService) WriteSharedDoc(name, content string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	p := d.store.Path("docs", "shared", filepath.Clean(name)+".md")
	d.store.EnsureDir("docs", "shared", filepath.Dir(filepath.Clean(name)))
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		return "", err
	}
	return name, nil
}

func (d *DocsService) ReadSharedDoc(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	p := d.store.Path("docs", "shared", filepath.Clean(name)+".md")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *DocsService) ListSharedDocs() ([]string, error) {
	dir := d.store.Path("docs", "shared")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d *DocsService) WriteIssueDoc(issueID, name, content string) (string, error) {
	if issueID == "" || name == "" {
		return "", fmt.Errorf("issue_id and name are required")
	}
	p := d.store.Path("issues", issueID, "docs", filepath.Clean(name)+".md")
	d.store.EnsureDir("issues", issueID, "docs", filepath.Dir(filepath.Clean(name)))
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		return "", err
	}
	return name, nil
}

func (d *DocsService) ReadIssueDoc(issueID, name string) (string, error) {
	if issueID == "" || name == "" {
		return "", fmt.Errorf("issue_id and name are required")
	}
	p := d.store.Path("issues", issueID, "docs", filepath.Clean(name)+".md")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *DocsService) ListIssueDocs(issueID string) ([]string, error) {
	if issueID == "" {
		return nil, fmt.Errorf("issue_id is required")
	}
	dir := d.store.Path("issues", issueID, "docs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func (d *DocsService) WriteTaskDoc(issueID, taskID, name, content string) (string, error) {
	if issueID == "" || taskID == "" || name == "" {
		return "", fmt.Errorf("issue_id, task_id and name are required")
	}
	p := d.store.Path("issues", issueID, "tasks", taskID+".docs", filepath.Clean(name)+".md")
	d.store.EnsureDir("issues", issueID, "tasks", taskID+".docs", filepath.Dir(filepath.Clean(name)))
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		return "", err
	}
	return name, nil
}

func (d *DocsService) ReadTaskDoc(issueID, taskID, name string) (string, error) {
	if issueID == "" || taskID == "" || name == "" {
		return "", fmt.Errorf("issue_id, task_id and name are required")
	}
	p := d.store.Path("issues", issueID, "tasks", taskID+".docs", filepath.Clean(name)+".md")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *DocsService) ListTaskDocs(issueID, taskID string) ([]string, error) {
	if issueID == "" || taskID == "" {
		return nil, fmt.Errorf("issue_id and task_id are required")
	}
	dir := d.store.Path("issues", issueID, "tasks", taskID+".docs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
