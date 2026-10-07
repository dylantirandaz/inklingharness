package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Config holds key=value settings.
type Config map[string]string

// GetCfg reads a file of key=value lines. Blank lines and lines that start
// with # are skipped.
func GetCfg(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	config := Config{}
	scanner := bufio.NewScanner(file)
	for number := 1; scanner.Scan(); number++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s:%d: missing =", path, number)
		}
		config[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return config, scanner.Err()
}
