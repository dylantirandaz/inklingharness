package fixture

import "fixture/config"

// Port returns the configured port, or 80.
func Port(path string) (string, error) {
	settings, err := config.GetCfg(path)
	if err != nil {
		return "", err
	}
	if port, found := settings["port"]; found {
		return port, nil
	}
	return "80", nil
}
