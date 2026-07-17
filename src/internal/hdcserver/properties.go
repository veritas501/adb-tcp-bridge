package hdcserver

import (
	"bufio"
	"strings"
)

func parseGetprop(output []byte) map[string]string {
	properties := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		key, value, ok := parseGetpropLine(scanner.Text())
		if ok {
			properties[key] = value
		}
	}
	return properties
}

func parseTargetListProperties(output []byte, serial string) (map[string]string, bool) {
	var first map[string]string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] == "[Empty]" {
			continue
		}
		if len(fields) < 4 {
			continue
		}
		props := fallbackProperties(fields[0], fields[3])
		if first == nil {
			first = props
		}
		if fields[0] == serial {
			return props, true
		}
	}
	if serial == "any" && first != nil {
		return first, true
	}
	return nil, false
}

func fallbackProperties(target string, devName string) map[string]string {
	model := devName
	if isGenericDevName(model) {
		model = "OpenHarmony"
	}
	return map[string]string{
		"ro.product.name":   "openharmony",
		"ro.product.model":  model,
		"ro.product.device": "openharmony",
	}
}

func isGenericDevName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "" || value == "localhost" || value == "unknown" || value == "unknown..."
}

func cleanParamValue(output []byte) string {
	value := strings.Trim(strings.TrimSpace(string(output)), "\x00")
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "get parameter") && strings.Contains(lower, "fail") {
		return ""
	}
	if strings.HasPrefix(lower, "fail") || strings.HasPrefix(lower, "error") {
		return ""
	}
	return value
}

func parseGetpropLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[") {
		return "", "", false
	}
	parts := strings.SplitN(line, "]: [", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimPrefix(parts[0], "[")
	value := strings.TrimSuffix(parts[1], "]")
	if key == "" {
		return "", "", false
	}
	return key, value, true
}
