package api

// coordinator.go は mgr の「全員で分担」を支える親子タスク操作を実装する（§16.7）。
//
//   - CreateSubtask         親タスクの子タスクを作成（配送・割当は mgr が行う）
//   - SubtaskStatuses       子タスクの状態一覧（完了集約用）
//   - ParentTaskID          子タスクから親タスク ID を引く

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/yukirawa/ai-office/server/internal/agents"
	"github.com/yukirawa/ai-office/server/internal/config"
	"github.com/yukirawa/ai-office/server/internal/store"
)

// CreateSubtask は agents.TaskCoordinator の実装。親タスクの子タスクを作成し、その ID を返す。
func (s *Server) CreateSubtask(_ context.Context, parent agents.Task, sub agents.Subtask) (string, error) {
	mode := parent.Mode
	if mode == "" {
		mode = "local"
	}
	now := time.Now().UTC()
	id := uuid.NewString()
	task := store.Task{
		ID:          id,
		ParentID:    parent.ID,
		Title:       sub.Title,
		Description: sub.Detail,
		Status:      "assigned",
		Assignee:    sub.Assignee,
		CreatedBy:   config.EmployeeManagerID,
		Mode:        mode,
		Repo:        parent.Repo,
		BaseBranch:  parent.BaseBranch,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.InsertTask(task); err != nil {
		return "", err
	}
	s.BroadcastState()
	s.log.Info("サブタスクを作成しました",
		"parent", parent.ID, "task_id", id, "assignee", sub.Assignee, "title", sub.Title)
	return id, nil
}

// SubtaskStatuses は agents.TaskCoordinator の実装。子タスクの状態一覧を返す。
func (s *Server) SubtaskStatuses(_ context.Context, parentID string) ([]agents.TaskStatus, error) {
	tasks, err := s.store.Subtasks(parentID)
	if err != nil {
		return nil, err
	}
	out := make([]agents.TaskStatus, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, agents.TaskStatus{ID: t.ID, Status: t.Status})
	}
	return out, nil
}

// ParentTaskID は agents.TaskCoordinator の実装。taskID の親タスク ID を返す（無ければ ""）。
func (s *Server) ParentTaskID(_ context.Context, taskID string) (string, error) {
	t, err := s.store.Task(taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return t.ParentID, nil
}
