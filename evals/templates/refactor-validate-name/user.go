package fixture

import (
	"errors"
	"strings"
)

// User is an account.
type User struct {
	Name  string
	Email string
}

// CreateUser checks the fields and returns a new user.
func CreateUser(name, email string) (User, error) {
	if strings.TrimSpace(name) == "" {
		return User{}, errors.New("name is empty")
	}
	if len(name) > 32 {
		return User{}, errors.New("name is longer than 32 bytes")
	}
	if strings.ContainsAny(name, "\n\t") {
		return User{}, errors.New("name contains a control character")
	}
	if !strings.Contains(email, "@") {
		return User{}, errors.New("email has no @")
	}
	return User{Name: name, Email: email}, nil
}

// RenameUser returns a copy of u with a new name.
func RenameUser(u User, name string) (User, error) {
	if strings.TrimSpace(name) == "" {
		return User{}, errors.New("name is empty")
	}
	if len(name) > 32 {
		return User{}, errors.New("name is longer than 32 bytes")
	}
	if strings.ContainsAny(name, "\n\t") {
		return User{}, errors.New("name contains a control character")
	}
	renamed := u
	renamed.Name = name
	return renamed, nil
}
