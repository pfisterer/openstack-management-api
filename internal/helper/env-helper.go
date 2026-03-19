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

func GetEnvStringArray(key string, defaultVal []string, sep string, to_lower bool) []string {
	if valStr := os.Getenv(key); valStr != "" {
		parts := strings.Split(valStr, sep)
		for i, part := range parts {
			parts[i] = strings.TrimSpace(part)
			if to_lower {
				parts[i] = strings.ToLower(parts[i])
			}
		}
		return parts
	}

	return defaultVal
}

func GetEnvStringSet(key string, defaultVal map[string]struct{}, sep string, to_lower bool) map[string]struct{} {
	if valStr := os.Getenv(key); valStr != "" {
		parts := strings.Split(valStr, sep)
		set := make(map[string]struct{}, len(parts))

		for _, part := range parts {
			part = strings.TrimSpace(part)
			if to_lower {
				part = strings.ToLower(part)
			}
			set[part] = struct{}{}
		}

		return set
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
