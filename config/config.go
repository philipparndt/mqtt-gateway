package config

import (
	"os"
	"regexp"
)

type MQTTConfig struct {
	URL      string `json:"url"`
	Retain   bool   `json:"retain"`
	Topic    string `json:"topic"`
	QoS      byte   `json:"qos"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func ReplaceEnvVariables(input []byte) []byte {
	envVariableRegex := regexp.MustCompile(`\${([^}]+)}`)

	return envVariableRegex.ReplaceAllFunc(input, func(match []byte) []byte {
		envVarName := match[2 : len(match)-1] // Extract the variable name without "${}".
		return []byte(os.Getenv(string(envVarName)))
	})
}
