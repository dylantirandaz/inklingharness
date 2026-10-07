package extend

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const settingsFileName = "settings.json"

// Settings is the merge of the user and the project settings files.
type Settings struct {
	// Allow and Deny are permission rule strings, user rules first.
	Allow, Deny []string
	Hooks       Hooks
	// MCPServers is sorted by Name. A project server replaces a user server
	// with the same name.
	MCPServers []MCPServer
}

// MCPServer is one MCP server that runs as a local process.
type MCPServer struct {
	Name, Command string
	Args          []string
	Env           map[string]string
	Description   string
}

// Hooks holds the hook commands of each event, user hooks first.
type Hooks struct {
	BeforeTool, AfterTool, Prompt []Hook
}

// Hook is one command that runs with bash -c.
type Hook struct {
	Command string
	// Tools limits the hook to these tool names. Empty means every tool.
	Tools []string
}

type settingsFile struct {
	Permissions struct {
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	} `json:"permissions"`
	Hooks struct {
		BeforeTool []hookFile `json:"before_tool"`
		AfterTool  []hookFile `json:"after_tool"`
		Prompt     []hookFile `json:"prompt"`
	} `json:"hooks"`
	MCPServers map[string]mcpServerFile `json:"mcp_servers"`
}

type hookFile struct {
	Command string   `json:"command"`
	Tools   []string `json:"tools"`
}

type mcpServerFile struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Description string            `json:"description"`
}

// LoadSettings reads settings.json from the user directory and from
// <workDir>/.inkling. A missing file is not an error.
func LoadSettings(workDir string) (Settings, error) {
	userDir, err := UserDir()
	if err != nil {
		return Settings{}, err
	}
	return loadSettings(userDir, workDir)
}

func loadSettings(userDir, workDir string) (Settings, error) {
	var settings Settings
	servers := map[string]MCPServer{}
	for _, dir := range layerDirs(userDir, workDir) {
		path := filepath.Join(dir, settingsFileName)
		content, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Settings{}, fmt.Errorf("extend: read %s: %w", path, err)
		}
		var file settingsFile
		if err := decodeStrict(path, content, &file); err != nil {
			return Settings{}, err
		}
		settings.Allow = append(settings.Allow, file.Permissions.Allow...)
		settings.Deny = append(settings.Deny, file.Permissions.Deny...)

		beforeTool, err := convertHooks(path, "before_tool", file.Hooks.BeforeTool, true)
		if err != nil {
			return Settings{}, err
		}
		afterTool, err := convertHooks(path, "after_tool", file.Hooks.AfterTool, true)
		if err != nil {
			return Settings{}, err
		}
		prompt, err := convertHooks(path, "prompt", file.Hooks.Prompt, false)
		if err != nil {
			return Settings{}, err
		}
		settings.Hooks.BeforeTool = append(settings.Hooks.BeforeTool, beforeTool...)
		settings.Hooks.AfterTool = append(settings.Hooks.AfterTool, afterTool...)
		settings.Hooks.Prompt = append(settings.Hooks.Prompt, prompt...)

		for name, server := range file.MCPServers {
			if strings.TrimSpace(name) == "" {
				return Settings{}, fmt.Errorf("extend: %s: mcp_servers has an empty name", path)
			}
			if strings.TrimSpace(server.Command) == "" {
				return Settings{}, fmt.Errorf("extend: %s: mcp_servers.%s: command is empty", path, name)
			}
			servers[name] = MCPServer{
				Name:        name,
				Command:     server.Command,
				Args:        server.Args,
				Env:         server.Env,
				Description: server.Description,
			}
		}
	}
	for _, server := range servers {
		settings.MCPServers = append(settings.MCPServers, server)
	}
	slices.SortFunc(settings.MCPServers, func(left, right MCPServer) int {
		return strings.Compare(left.Name, right.Name)
	})
	return settings, nil
}

// convertHooks rejects a tools filter on events that have no tool, because
// such a filter could never match and would hide a mistake.
func convertHooks(path, event string, files []hookFile, takesTools bool) ([]Hook, error) {
	hooks := make([]Hook, 0, len(files))
	for index, file := range files {
		if strings.TrimSpace(file.Command) == "" {
			return nil, fmt.Errorf("extend: %s: hooks.%s[%d]: command is empty", path, event, index)
		}
		if !takesTools && len(file.Tools) > 0 {
			return nil, fmt.Errorf("extend: %s: hooks.%s[%d]: %s hooks do not take tools", path, event, index, event)
		}
		if slices.Contains(file.Tools, "") {
			return nil, fmt.Errorf("extend: %s: hooks.%s[%d]: tools has an empty name", path, event, index)
		}
		hooks = append(hooks, Hook{Command: file.Command, Tools: file.Tools})
	}
	return hooks, nil
}
