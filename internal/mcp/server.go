package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cookchen233/swarm-mcp/internal/swarm"
)

type ServerConfig struct {
	Name                  string
	Version               string
	Logger                *log.Logger
	Strict                bool
	SuggestedMinTaskCount int
	MaxTaskCount          int
	IssueTTLSec           int
	TaskTTLSec            int
	DefaultTimeoutSec     int
}

type Server struct {
	cfg ServerConfig
	in  io.Reader
	out io.Writer

	encMu sync.Mutex

	defaultMemberID string

	sessMu   sync.Mutex
	sessions map[string]string // session_id -> member_id

	docsSvc   *swarm.DocsService
	workerSvc *swarm.WorkerService
	lockSvc   *swarm.LockService
	issueSvc  *swarm.IssueService
}

func NewServer(cfg ServerConfig, store *swarm.Store, trace *swarm.TraceService) *Server {
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "swarm-mcp: ", log.LstdFlags|log.LUTC)
	}
	return &Server{
		cfg:             cfg,
		in:              os.Stdin,
		out:             os.Stdout,
		defaultMemberID: swarm.GenID("m"),
		sessions:        map[string]string{},
		docsSvc:         swarm.NewDocsService(store),
		workerSvc:       swarm.NewWorkerService(store, trace),
		lockSvc:         swarm.NewLockService(store, trace),
		issueSvc:        swarm.NewIssueService(store, trace, cfg.IssueTTLSec, cfg.TaskTTLSec, cfg.DefaultTimeoutSec),
	}
}

func (s *Server) Run() error {
	s.cfg.Logger.Printf("starting %s %s", s.cfg.Name, s.cfg.Version)

	scanner := bufio.NewScanner(s.in)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)

	enc := json.NewEncoder(s.out)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.encMu.Lock()
			_ = enc.Encode(NewErrorResponse(nil, ErrParse, "invalid JSON", err.Error()))
			s.encMu.Unlock()
			continue
		}

		// IMPORTANT: handle requests concurrently so long-poll calls do not block other tools.
		go func(req JSONRPCRequest) {
			resp := s.handle(req)
			if resp == nil {
				return
			}

			s.encMu.Lock()
			defer s.encMu.Unlock()
			_ = enc.Encode(resp)
		}(req)
	}

	return scanner.Err()
}

func (s *Server) memberIDForArgs(toolName string, args map[string]any) (string, error) {
	// Strong constraint: all tools MUST carry a valid session_id, except openSession.
	if toolName == "openSession" {
		return s.defaultMemberID, nil
	}
	if args == nil {
		return "", fmt.Errorf("session_id is required")
	}
	sessionID, _ := args["session_id"].(string)
	if sessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if mid, ok := s.sessions[sessionID]; ok {
		return mid, nil
	}
	return "", fmt.Errorf("unknown session_id")
}

func (s *Server) openSession() (sessionID, memberID string) {
	sessionID = swarm.GenID("sess")
	memberID = swarm.GenID("m")
	s.sessMu.Lock()
	s.sessions[sessionID] = memberID
	s.sessMu.Unlock()
	return sessionID, memberID
}

func (s *Server) handle(req JSONRPCRequest) *JSONRPCResponse {
	if req.ID == nil {
		return nil
	}

	switch req.Method {
	case "initialize":
		resp := s.handleInitialize(req.ID)
		return &resp
	case "prompts/list":
		resp := NewResultResponse(req.ID, map[string]any{"prompts": []any{}})
		return &resp
	case "resources/list":
		resp := NewResultResponse(req.ID, map[string]any{"resources": []any{}})
		return &resp
	case "tools/list":
		resp := NewResultResponse(req.ID, map[string]any{"tools": allToolsStrict(s.cfg.Strict)})
		return &resp
	case "tools/call":
		resp := s.handleToolsCall(req.ID, req.Params)
		return &resp
	default:
		resp := NewErrorResponse(req.ID, ErrMethodNotFound, "method not found", nil)
		return &resp
	}
}

func (s *Server) handleInitialize(id any) JSONRPCResponse {
	return NewResultResponse(id, map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"resources": map[string]any{},
			"prompts":   map[string]any{},
			"tools":     map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    s.cfg.Name,
			"version": s.cfg.Version,
		},
	})
}

func (s *Server) handleToolsCall(id any, params any) JSONRPCResponse {
	paramsMap, ok := params.(map[string]any)
	if !ok {
		return NewErrorResponse(id, ErrInvalidParams, "invalid params", nil)
	}

	name, _ := paramsMap["name"].(string)
	args := map[string]any{}
	if a, ok := paramsMap["arguments"].(map[string]any); ok {
		args = a
	}

	result, err := s.dispatch(name, args)
	if err != nil {
		return NewResultResponse(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("ERROR: %v", err)}},
			"isError": true,
		})
	}

	resultJSON, _ := json.MarshalIndent(result, "", "  ")
	return NewResultResponse(id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(resultJSON)}},
	})
}

func (s *Server) dispatch(name string, args map[string]any) (any, error) {
	memberID, err := s.memberIDForArgs(name, args)
	if err != nil {
		return nil, err
	}
	nowMs := time.Now().UnixMilli()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	toMap := func(v any) (map[string]any, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		return m, nil
	}
	addNow := func(m map[string]any) map[string]any {
		m["server_now_ms"] = nowMs
		m["server_now"] = nowStr
		return m
	}
	addLeaseExpiresAt := func(m map[string]any) map[string]any {
		if v, ok := m["lease_expires_at_ms"].(float64); ok {
			ms := int64(v)
			if ms > 0 {
				m["lease_expires_at"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
			} else {
				m["lease_expires_at"] = ""
			}
		}
		return m
	}

	filterIssues := func(issues []swarm.Issue, status, subjectContains string) []swarm.Issue {
		out := make([]swarm.Issue, 0, len(issues))
		status = strings.TrimSpace(strings.ToLower(status))
		if status == "" {
			status = "all"
		}
		subjectContains = strings.TrimSpace(subjectContains)
		subjectContainsLower := strings.ToLower(subjectContains)
		for _, it := range issues {
			if status != "all" && status != "" {
				if it.Status != status {
					continue
				}
			}
			if subjectContainsLower != "" {
				if !strings.Contains(strings.ToLower(it.Subject), subjectContainsLower) {
					continue
				}
			}
			out = append(out, it)
		}
		return out
	}

	sortIssues := func(issues []swarm.Issue, sortBy, sortOrder string) {
		sortBy = strings.TrimSpace(strings.ToLower(sortBy))
		if sortBy == "" {
			sortBy = "created_at"
		}
		sortOrder = strings.TrimSpace(strings.ToLower(sortOrder))
		if sortOrder == "" {
			sortOrder = "desc"
		}
		less := func(i, j int) bool {
			var a, b string
			switch sortBy {
			case "updated_at":
				a, b = issues[i].UpdatedAt, issues[j].UpdatedAt
			default:
				a, b = issues[i].CreatedAt, issues[j].CreatedAt
			}
			// RFC3339 lexicographic compares correctly
			if sortOrder == "asc" {
				return a < b
			}
			return a > b
		}
		sort.SliceStable(issues, less)
	}

	paginateIssues := func(issues []swarm.Issue, offset, limit int) []swarm.Issue {
		if offset < 0 {
			offset = 0
		}
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		if offset >= len(issues) {
			return []swarm.Issue{}
		}
		end := offset + limit
		if end > len(issues) {
			end = len(issues)
		}
		return issues[offset:end]
	}

	filterTasks := func(tasks []swarm.IssueTask, status, subjectContains, claimedBy, submitter string) []swarm.IssueTask {
		out := make([]swarm.IssueTask, 0, len(tasks))
		status = strings.TrimSpace(strings.ToLower(status))
		if status == "" {
			status = "all"
		}
		subjectContains = strings.TrimSpace(subjectContains)
		subjectContainsLower := strings.ToLower(subjectContains)
		claimedBy = strings.TrimSpace(claimedBy)
		submitter = strings.TrimSpace(submitter)
		for _, it := range tasks {
			if status != "all" && status != "" {
				if it.Status != status {
					continue
				}
			}
			if subjectContainsLower != "" {
				if !strings.Contains(strings.ToLower(it.Subject), subjectContainsLower) {
					continue
				}
			}
			if claimedBy != "" {
				if it.ClaimedBy != claimedBy {
					continue
				}
			}
			if submitter != "" {
				if it.Submitter != submitter {
					continue
				}
			}
			out = append(out, it)
		}
		return out
	}

	sortTasks := func(tasks []swarm.IssueTask, sortBy, sortOrder string) {
		sortBy = strings.TrimSpace(strings.ToLower(sortBy))
		if sortBy == "" {
			sortBy = "created_at"
		}
		sortOrder = strings.TrimSpace(strings.ToLower(sortOrder))
		if sortOrder == "" {
			sortOrder = "desc"
		}
		less := func(i, j int) bool {
			switch sortBy {
			case "updated_at":
				if sortOrder == "asc" {
					return tasks[i].UpdatedAt < tasks[j].UpdatedAt
				}
				return tasks[i].UpdatedAt > tasks[j].UpdatedAt
			case "points":
				if sortOrder == "asc" {
					return tasks[i].Points < tasks[j].Points
				}
				return tasks[i].Points > tasks[j].Points
			default:
				if sortOrder == "asc" {
					return tasks[i].CreatedAt < tasks[j].CreatedAt
				}
				return tasks[i].CreatedAt > tasks[j].CreatedAt
			}
		}
		sort.SliceStable(tasks, less)
	}

	paginateTasks := func(tasks []swarm.IssueTask, offset, limit int) []swarm.IssueTask {
		if offset < 0 {
			offset = 0
		}
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		if offset >= len(tasks) {
			return []swarm.IssueTask{}
		}
		end := offset + limit
		if end > len(tasks) {
			end = len(tasks)
		}
		return tasks[offset:end]
	}
	switch name {
	case "whoAmI":
		return map[string]any{"member_id": memberID}, nil
	case "swarmNow":
		return map[string]any{"now_ms": nowMs, "now": nowStr}, nil
	case "openSession":
		sid, mid := s.openSession()
		return map[string]any{"session_id": sid, "member_id": mid}, nil

	// === Issue pool ===
	case "listIssues":
		issues, err := s.issueSvc.ListIssues()
		if err != nil {
			return nil, err
		}
		issues = filterIssues(issues, str(args, "status"), str(args, "subject_contains"))
		sortIssues(issues, str(args, "sort_by"), str(args, "sort_order"))
		issues = paginateIssues(issues, intVal(args, "offset"), intVal(args, "limit"))
		out := make([]map[string]any, 0, len(issues))
		for _, it := range issues {
			m := map[string]any{
				"id":                  it.ID,
				"subject":             it.Subject,
				"status":              it.Status,
				"lease_expires_at_ms": it.LeaseExpiresAtMs,
				"created_at":          it.CreatedAt,
				"updated_at":          it.UpdatedAt,
			}
			out = append(out, addLeaseExpiresAt(m))
		}
		return out, nil
	case "listOpenedIssues":
		issues, err := s.issueSvc.ListIssues()
		if err != nil {
			return nil, err
		}
		issues = filterIssues(issues, swarm.IssueOpen, "")
		sortIssues(issues, "created_at", "desc")
		out := make([]map[string]any, 0, len(issues))
		for _, it := range issues {
			m := map[string]any{
				"id":                  it.ID,
				"subject":             it.Subject,
				"status":              it.Status,
				"lease_expires_at_ms": it.LeaseExpiresAtMs,
				"created_at":          it.CreatedAt,
				"updated_at":          it.UpdatedAt,
			}
			out = append(out, addLeaseExpiresAt(m))
		}
		return out, nil
	case "waitIssues":
		status := str(args, "status")
		if strings.TrimSpace(status) == "" {
			status = swarm.IssueOpen
		}
		issues, err := s.issueSvc.WaitIssues(status, timeoutWithMin(intVal(args, "timeout_sec"), s.cfg.DefaultTimeoutSec), intVal(args, "limit"))
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(issues))
		for _, it := range issues {
			m, err := toMap(it)
			if err != nil {
				return nil, err
			}
			out = append(out, addLeaseExpiresAt(addNow(m)))
		}
		return map[string]any{"issues": out, "count": len(issues), "server_now_ms": nowMs, "server_now": nowStr}, nil
	case "waitIssueTasks":
		status := str(args, "status")
		if strings.TrimSpace(status) == "" {
			status = swarm.IssueTaskOpen
		}
		tasks, err := s.issueSvc.WaitIssueTasks(str(args, "issue_id"), status, timeoutWithMin(intVal(args, "timeout_sec"), s.cfg.DefaultTimeoutSec), intVal(args, "limit"))
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(tasks))
		for _, it := range tasks {
			m, err := toMap(it)
			if err != nil {
				return nil, err
			}
			out = append(out, addLeaseExpiresAt(addNow(m)))
		}
		return map[string]any{"tasks": out, "count": len(tasks), "server_now_ms": nowMs, "server_now": nowStr}, nil
	case "getIssue":
		issue, err := s.issueSvc.GetIssue(str(args, "issue_id"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "extendIssueLease":
		issue, err := s.issueSvc.ExtendIssueLease(memberID, str(args, "issue_id"), intVal(args, "extend_sec"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "extendIssueTaskLease":
		task, err := s.issueSvc.ExtendIssueTaskLease(memberID, str(args, "issue_id"), str(args, "task_id"), intVal(args, "extend_sec"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "closeIssue":
		issue, err := s.issueSvc.CloseIssue(memberID, str(args, "issue_id"), str(args, "summary"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "reopenIssue":
		issue, err := s.issueSvc.ReopenIssue(memberID, str(args, "issue_id"), str(args, "summary"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "deliverDelivery":
		art := objMap(args, "artifacts")
		out, err := s.issueSvc.DeliverAndWaitReview(
			memberID,
			str(args, "issue_id"),
			str(args, "summary"),
			str(args, "refs"),
			swarm.DeliveryArtifacts{
				TestResult:   str(art, "test_result"),
				TestCases:    strSlice(art, "test_cases"),
				ChangedFiles: strSlice(art, "changed_files"),
				ReviewedRefs: strSlice(art, "reviewed_refs"),
				TestOutput:   str(art, "test_output"),
				KnownRisks:   str(art, "known_risks"),
			},
			timeoutWithMin(intVal(args, "timeout_sec"), s.cfg.DefaultTimeoutSec),
		)
		if err != nil {
			return nil, err
		}
		return addNow(out), nil
	case "claimDelivery":
		d, err := s.issueSvc.ClaimDelivery(memberID, str(args, "delivery_id"), intVal(args, "extend_sec"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(d)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "extendDeliveryLease":
		d, err := s.issueSvc.ExtendDeliveryLease(memberID, str(args, "delivery_id"), intVal(args, "extend_sec"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(d)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "reviewDelivery":
		d, err := s.issueSvc.ReviewDelivery(memberID, str(args, "delivery_id"), str(args, "verdict"), str(args, "feedback"), str(args, "refs"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(d)
		if err != nil {
			return nil, err
		}
		return addNow(m), nil
	case "getDelivery":
		d, err := s.issueSvc.GetDelivery(str(args, "delivery_id"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(d)
		if err != nil {
			return nil, err
		}
		return addNow(m), nil
	case "listDeliveries":
		ds, err := s.issueSvc.ListDeliveries(
			str(args, "status"),
			str(args, "issue_id"),
			str(args, "delivered_by"),
			str(args, "reviewed_by"),
		)
		if err != nil {
			return nil, err
		}
		offset := intVal(args, "offset")
		limit := intVal(args, "limit")
		if offset < 0 {
			offset = 0
		}
		if limit <= 0 {
			limit = 50
		}
		if limit > 200 {
			limit = 200
		}
		if offset > len(ds) {
			offset = len(ds)
		}
		end := offset + limit
		if end > len(ds) {
			end = len(ds)
		}
		ds = ds[offset:end]
		out := make([]map[string]any, 0, len(ds))
		for _, it := range ds {
			m, err := toMap(it)
			if err != nil {
				return nil, err
			}
			out = append(out, addNow(m))
		}
		return out, nil
	case "listOpenedDeliveries":
		ds, err := s.issueSvc.ListDeliveries(swarm.DeliveryOpen, "", "", "")
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(ds))
		for _, it := range ds {
			m, err := toMap(it)
			if err != nil {
				return nil, err
			}
			out = append(out, addNow(m))
		}
		return out, nil
	case "waitDeliveries":
		status := str(args, "status")
		if strings.TrimSpace(status) == "" {
			status = swarm.DeliveryOpen
		}
		ds, err := s.issueSvc.WaitDeliveries(status, timeoutWithMin(intVal(args, "timeout_sec"), s.cfg.DefaultTimeoutSec), intVal(args, "limit"))
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, len(ds))
		for _, it := range ds {
			m, err := toMap(it)
			if err != nil {
				return nil, err
			}
			out = append(out, addLeaseExpiresAt(addNow(m)))
		}
		return map[string]any{"deliveries": out, "count": len(ds), "server_now_ms": nowMs, "server_now": nowStr}, nil
	case "getIssueAcceptanceBundle":
		issueID := str(args, "issue_id")
		issue, err := s.issueSvc.GetIssue(issueID)
		if err != nil {
			return nil, err
		}
		tasks, err := s.issueSvc.ListTasks(issueID, "")
		if err != nil {
			return nil, err
		}
		issueDocs := map[string]string{}
		for _, d := range issue.Docs {
			c, err := s.docsSvc.ReadIssueDoc(issueID, d.Name)
			if err != nil {
				return nil, err
			}
			issueDocs[d.Name] = c
		}
		taskDocs := map[string]map[string]string{}
		for _, t := range tasks {
			m := map[string]string{}
			for _, d := range t.TaskDocs {
				c, err := s.docsSvc.ReadTaskDoc(issueID, t.ID, d.Name)
				if err != nil {
					return nil, err
				}
				m[d.Name] = c
			}
			taskDocs[t.ID] = m
		}
		events, err := s.issueSvc.ReadAllEvents(issueID)
		if err != nil {
			return nil, err
		}
		issueMap, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		issueMap = addLeaseExpiresAt(addNow(issueMap))
		bundle := map[string]any{
			"issue":      issueMap,
			"tasks":      tasks,
			"issue_docs": issueDocs,
			"task_docs":  taskDocs,
			"events":     events,
		}
		return bundle, nil

	// === Issue / Task (issue-centric default) ===
	case "createIssue":
		userName, userContent := docObj(args, "user_issue_doc")
		leadName, leadContent := docObj(args, "lead_issue_doc")
		otherDocs := docObjSlice(args, "user_other_docs")
		issue, err := s.issueSvc.CreateIssue(
			memberID,
			str(args, "subject"),
			str(args, "description"),
			strSlice(args, "shared_doc_paths"),
			strSlice(args, "project_doc_paths"),
			userName,
			userContent,
			leadName,
			leadContent,
			otherDocs,
		)
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "updateIssueDocPaths":
		issue, err := s.issueSvc.UpdateIssueDocPaths(
			memberID,
			str(args, "issue_id"),
			strSlice(args, "shared_doc_paths"),
			strSlice(args, "project_doc_paths"),
		)
		if err != nil {
			return nil, err
		}
		m, err := toMap(issue)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "createIssueTask":
		if s.cfg.MaxTaskCount > 0 {
			cnt, err := s.issueSvc.CountTasks(str(args, "issue_id"))
			if err != nil {
				return nil, err
			}
			if cnt >= s.cfg.MaxTaskCount {
				return nil, fmt.Errorf("max_task_count exceeded: %d", s.cfg.MaxTaskCount)
			}
		}
		spec := objMap(args, "spec")
		task, err := s.issueSvc.CreateTask(
			memberID,
			str(args, "issue_id"),
			str(args, "subject"),
			str(args, "description"),
			str(args, "difficulty"),
			strSlice(args, "suggested_files"),
			strSlice(args, "labels"),
			strSlice(args, "doc_paths"),
			intVal(args, "points"),
			strSlice(args, "context_task_ids"),
			str(spec, "name"),
			str(spec, "split_from"),
			str(spec, "split_reason"),
			str(spec, "impact_scope"),
			strSlice(spec, "context_task_ids"),
			str(spec, "goal"),
			str(spec, "rules"),
			str(spec, "constraints"),
			str(spec, "conventions"),
			str(spec, "acceptance"),
		)
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "claimIssueTask":
		_, _ = s.workerSvc.Register(memberID)
		task, err := s.issueSvc.ClaimTask(str(args, "issue_id"), str(args, "task_id"), memberID, str(args, "next_step_token"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "submitIssueTask":
		art := objMap(args, "artifacts")
		task, err := s.issueSvc.SubmitTask(
			str(args, "issue_id"),
			str(args, "task_id"),
			memberID,
			swarm.SubmissionArtifacts{
				Summary:      str(art, "summary"),
				ChangedFiles: strSlice(art, "changed_files"),
				Diff:         str(art, "diff"),
				Links:        strSlice(art, "links"),
				TestCases:    strSlice(art, "test_cases"),
				TestResult:   str(art, "test_result"),
				TestOutput:   str(art, "test_output"),
			},
		)
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		actions := make([]map[string]any, 0, 2)
		actions = append(actions, map[string]any{
			"tool": "listIssueTasks",
			"args": map[string]any{"issue_id": task.IssueID, "status": "open"},
			"note": "List open tasks. Reserved tasks include reserved_token.",
		})
		if task.NextStepToken != "" {
			if tok, err := s.issueSvc.ReadNextStepToken(task.IssueID, task.NextStepToken); err == nil {
				if tok.NextStep.Type == "claim_task" && tok.NextStep.TaskID != "" {
					actions = append(actions, map[string]any{
						"tool": "claimIssueTask",
						"args": map[string]any{"issue_id": task.IssueID, "task_id": tok.NextStep.TaskID, "next_step_token": task.NextStepToken},
						"note": "Claim the reserved next-step task.",
					})
				}
			}
		}
		m["next_actions"] = actions
		return addLeaseExpiresAt(addNow(m)), nil
	case "reviewIssueTask":
		art := objMap(args, "artifacts")
		fds := mapSlice(args, "feedback_details")
		feedbackDetails := make([]swarm.FeedbackDetail, 0, len(fds))
		for _, fd := range fds {
			feedbackDetails = append(feedbackDetails, swarm.FeedbackDetail{
				Dimension:  str(fd, "dimension"),
				Severity:   str(fd, "severity"),
				FilePath:   str(fd, "file_path"),
				LineRange:  str(fd, "line_range"),
				Content:    str(fd, "content"),
				Suggestion: str(fd, "suggestion"),
			})
		}
		task, err := s.issueSvc.ReviewTask(
			memberID,
			str(args, "issue_id"),
			str(args, "task_id"),
			str(args, "verdict"),
			str(args, "feedback"),
			intVal(args, "completion_score"),
			swarm.ReviewArtifacts{
				ReviewSummary: str(art, "review_summary"),
				ReviewedRefs:  strSlice(art, "reviewed_refs"),
			},
			feedbackDetails,
			str(args, "next_step_token"),
		)
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "getNextStepToken":
		return s.issueSvc.GetNextStepToken(
			str(args, "issue_id"),
			memberID,
			str(args, "task_id"),
			str(args, "worker_id"),
			intVal(args, "completion_score"),
		)
	case "getIssueTask":
		task, err := s.issueSvc.GetTask(str(args, "issue_id"), str(args, "task_id"))
		if err != nil {
			return nil, err
		}
		m, err := toMap(task)
		if err != nil {
			return nil, err
		}
		return addLeaseExpiresAt(addNow(m)), nil
	case "listIssueTasks":
		issueID := str(args, "issue_id")
		tasks, err := s.issueSvc.ListTasks(issueID, "")
		if err != nil {
			return nil, err
		}
		tasks = filterTasks(tasks, str(args, "status"), str(args, "subject_contains"), str(args, "claimed_by"), str(args, "submitter"))
		sortTasks(tasks, str(args, "sort_by"), str(args, "sort_order"))
		tasks = paginateTasks(tasks, intVal(args, "offset"), intVal(args, "limit"))
		out := make([]map[string]any, 0, len(tasks))
		for _, it := range tasks {
			m := map[string]any{
				"id":                  it.ID,
				"issue_id":            it.IssueID,
				"subject":             it.Subject,
				"difficulty":          it.Difficulty,
				"points":              it.Points,
				"status":              it.Status,
				"reserved_token":      it.ReservedToken,
				"reserved_until_ms":   it.ReservedUntilMs,
				"lease_expires_at_ms": it.LeaseExpiresAtMs,
				"claimed_by":          it.ClaimedBy,
				"created_at":          it.CreatedAt,
				"updated_at":          it.UpdatedAt,
			}
			out = append(out, addLeaseExpiresAt(m))
		}
		return out, nil
	case "listIssueOpenedTasks":
		issueID := str(args, "issue_id")
		tasks, err := s.issueSvc.ListTasks(issueID, swarm.IssueTaskOpen)
		if err != nil {
			return nil, err
		}
		sortTasks(tasks, "created_at", "desc")
		out := make([]map[string]any, 0, len(tasks))
		for _, it := range tasks {
			m := map[string]any{
				"id":                  it.ID,
				"issue_id":            it.IssueID,
				"subject":             it.Subject,
				"difficulty":          it.Difficulty,
				"points":              it.Points,
				"status":              it.Status,
				"reserved_token":      it.ReservedToken,
				"reserved_until_ms":   it.ReservedUntilMs,
				"lease_expires_at_ms": it.LeaseExpiresAtMs,
				"claimed_by":          it.ClaimedBy,
				"created_at":          it.CreatedAt,
				"updated_at":          it.UpdatedAt,
			}
			out = append(out, addLeaseExpiresAt(m))
		}
		return out, nil
	case "waitIssueTaskEvents":
		// Lead passive mode: only issue_id is honored.
		// Cursor auto-resumes per (issue_id, member_id).
		after := int64(-1)
		timeoutSec := s.cfg.DefaultTimeoutSec
		limit := 50
		events, nextSeq, err := s.issueSvc.WaitIssueTaskEvents(
			str(args, "issue_id"),
			memberID,
			after,
			timeoutSec,
			limit,
		)
		if err != nil {
			return nil, err
		}
		if len(events) > 1 {
			events = events[:1]
		}
		return map[string]any{"events": events, "next_seq": nextSeq}, nil
	case "askIssueTask":
		return s.issueSvc.AskIssueTask(
			str(args, "issue_id"),
			str(args, "task_id"),
			memberID,
			str(args, "kind"),
			str(args, "content"),
			str(args, "refs"),
			timeoutWithMin(intVal(args, "timeout_sec"), s.cfg.DefaultTimeoutSec),
		)
	case "postIssueTaskMessage":
		return s.issueSvc.PostTaskMessage(
			str(args, "issue_id"),
			str(args, "task_id"),
			memberID,
			str(args, "kind"),
			str(args, "content"),
			str(args, "refs"),
		)
	case "replyIssueTaskMessage":
		return s.issueSvc.ReplyTaskMessage(
			str(args, "issue_id"),
			str(args, "task_id"),
			memberID,
			str(args, "content"),
			str(args, "refs"),
		)

	// === Workers ===
	case "registerWorker":
		return s.workerSvc.Register(strOr(args, "worker_id", memberID))
	case "listWorkers":
		return s.workerSvc.List()
	case "getWorker":
		return s.workerSvc.Get(str(args, "worker_id"))

	// === Docs ===
	case "writeSharedDoc":
		return s.docsSvc.WriteSharedDoc(str(args, "name"), str(args, "content"))
	case "readSharedDoc":
		return s.docsSvc.ReadSharedDoc(str(args, "name"))
	case "listSharedDocs":
		return s.docsSvc.ListSharedDocs()
	case "writeIssueDoc":
		return s.docsSvc.WriteIssueDoc(str(args, "issue_id"), str(args, "name"), str(args, "content"))
	case "readIssueDoc":
		return s.docsSvc.ReadIssueDoc(str(args, "issue_id"), str(args, "name"))
	case "listIssueDocs":
		return s.docsSvc.ListIssueDocs(str(args, "issue_id"))
	case "writeTaskDoc":
		return s.docsSvc.WriteTaskDoc(str(args, "issue_id"), str(args, "task_id"), str(args, "name"), str(args, "content"))
	case "readTaskDoc":
		return s.docsSvc.ReadTaskDoc(str(args, "issue_id"), str(args, "task_id"), str(args, "name"))
	case "listTaskDocs":
		return s.docsSvc.ListTaskDocs(str(args, "issue_id"), str(args, "task_id"))

	// Lock
	case "lockFiles":
		return s.lockSvc.LockFiles(
			str(args, "task_id"), strOr(args, "owner", memberID),
			strSlice(args, "files"), intVal(args, "ttl_sec"), intVal(args, "wait_sec"),
		)
	case "heartbeat":
		return s.lockSvc.Heartbeat(str(args, "lease_id"), intVal(args, "extend_sec"))
	case "unlock":
		return nil, s.lockSvc.Unlock(str(args, "lease_id"))
	case "listLocks":
		return s.lockSvc.ListLocks(str(args, "owner"), strSlice(args, "files"))
	case "forceUnlock":
		return nil, s.lockSvc.ForceUnlock(str(args, "lease_id"), str(args, "reason"))

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

// Argument extraction helpers
func str(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func strOr(args map[string]any, key, fallback string) string {
	if v := str(args, key); v != "" {
		return v
	}
	return fallback
}

func strSlice(args map[string]any, key string) []string {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func mapSlice(args map[string]any, key string) []map[string]any {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		result = append(result, m)
	}
	return result
}

func intVal(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func int64Val(args map[string]any, key string) int64 {
	switch v := args[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

// timeoutWithMin enforces a minimum timeout of 3600s (1 hour)
func timeoutWithMin(timeoutSec int, minTimeoutSec int) int {
	if timeoutSec <= 0 {
		return minTimeoutSec
	}
	if timeoutSec < minTimeoutSec {
		return minTimeoutSec
	}
	return timeoutSec
}

func objMap(args map[string]any, key string) map[string]any {
	v, _ := args[key].(map[string]any)
	return v
}

func docObj(args map[string]any, key string) (name, content string) {
	v := objMap(args, key)
	return str(v, "name"), str(v, "content")
}

func docObjSlice(args map[string]any, key string) []map[string]any {
	raw, ok := args[key].([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		result = append(result, m)
	}
	return result
}
