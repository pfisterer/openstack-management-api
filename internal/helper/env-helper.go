package helper

import (
	"os"
	"strconv"
	"strings"
)

func GetEnvString(key string, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return strings.TrimSpace(val)
	}

	return defaultVal
}

func GetEnvInt(key string, defaultVal int) int {

	if valStr := os.Getenv(key); valStr != "" {
		val, err := strconv.Atoi(strings.TrimSpace(valStr))

		if err == nil {
			return val
		}

	}

	return defaultVal
}

func GetEnvBool(key string, defaultVal bool) bool {
	if valStr := os.Getenv(key); valStr != "" {
		val, err := strconv.ParseBool(valStr)

		if err == nil {
			return val
		}

	}

	return defaultVal
}
