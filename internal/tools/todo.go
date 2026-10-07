package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type todoItem struct {
	Content *string `json:"content"`
	Status  string  `json:"status"`
}

func todoWriteTool() Tool {
	var mutex sync.Mutex
	var items []todoItem
	return Tool{
		Name:        "todo_write",
		Description: "Replace this tool set's task list with items containing content and status (pending, in_progress, completed). At most one task may be in_progress. An empty items array clears the list. State is local to this tool set, not written to disk. Output is capped at 256 KiB.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"content":{"type":"string"},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["content","status"]}}},"required":["items"]}`),
		ReadOnly:    false,
		Run: func(ctx context.Context, input json.RawMessage) (Result, error) {
			var arguments struct {
				Items *[]todoItem `json:"items"`
			}
			if err := json.Unmarshal(input, &arguments); err != nil {
				return invalidInput(err), nil
			}
			if arguments.Items == nil {
				return invalidInput(errors.New("items is required and must be an array")), nil
			}
			active := 0
			for _, item := range *arguments.Items {
				if err := ctx.Err(); err != nil {
					return Result{}, err
				}
				if item.Content == nil {
					return invalidInput(errors.New("each item requires string content")), nil
				}
				switch item.Status {
				case "pending", "completed":
				case "in_progress":
					active++
				default:
					return invalidInput(errors.New("status must be pending, in_progress, or completed")), nil
				}
			}
			if active > 1 {
				return invalidInput(errors.New("at most one item may be in_progress")), nil
			}
			mutex.Lock()
			defer mutex.Unlock()
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			items = *arguments.Items
			output := searchOutput{limit: len(items)}
			for index, item := range items {
				if !output.add(fmt.Sprintf("%d. [%s] %s", index+1, item.Status, *item.Content)) {
					break
				}
			}
			return output.result(false, fmt.Sprintf("[task list replaced: %d items]", len(items))), nil
		},
	}
}
