package notice

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// HookRegistrationStatus reports whether hooks.json contains any managed
// registration and whether the complete managed set matches this binary. It
// only reads the file; unrelated registrations are ignored. A custom handler
// executable is accepted when its command still invokes notice-hook with the
// configured notice path.
func HookRegistrationStatus(configDir, noticePath string) (managed, current bool, err error) {
	if noticePath == "" {
		var err error
		noticePath, err = ResolvePath("")
		if err != nil {
			return false, false, err
		}
	}
	entries, _, err := readHooksDoc(filepath.Join(configDir, "hooks.json"))
	if err != nil {
		return false, false, err
	}
	byName := make(map[string][]hookRegistration)
	for _, raw := range entries {
		var entry hookRegistration
		if json.Unmarshal(raw, &entry) == nil && isManagedHook(entry.Name) {
			byName[entry.Name] = append(byName[entry.Name], entry)
		}
	}
	managed = len(byName) > 0
	if !managed {
		return false, false, nil
	}
	for _, expected := range managedHookRegistrations("", noticePath) {
		matches := byName[expected.Name]
		if len(matches) != 1 || matches[0].Event != expected.Event || matches[0].Handler == nil || !managedHandlerMatches(matches[0].Handler.Bash, noticePath) {
			return true, false, nil
		}
	}
	return true, true, nil
}

type hookRegistration struct {
	Name    string `json:"name"`
	Event   string `json:"event"`
	Handler *struct {
		Bash string `json:"bash"`
	} `json:"handler"`
}

func isManagedHook(name string) bool {
	return name == reloadHookName || name == driftHookName || name == baselineHookName
}

func managedHandlerMatches(command, noticePath string) bool {
	if !strings.HasPrefix(command, "exec '") {
		return false
	}
	suffix := "' notice-hook --notice " + shellQuote(noticePath)
	if !strings.HasSuffix(command, suffix) {
		return false
	}
	handler := strings.TrimSuffix(strings.TrimPrefix(command, "exec '"), suffix)
	// Accept exactly the shell-quoted form produced by the installer, not
	// arbitrary command fragments that happen to end in the expected arguments.
	return handler != "" && shellQuote(strings.NewReplacer("'\\''", "'").Replace(handler)) == "'"+handler+"'"
}

func managedHookRegistrations(handlerPath, noticePath string) []hookRegistration {
	bash := "exec " + shellQuote(handlerPath) + " notice-hook --notice " + shellQuote(noticePath)
	return []hookRegistration{
		{Name: reloadHookName, Event: "post_model_turn", Handler: &struct {
			Bash string `json:"bash"`
		}{Bash: bash}},
		{Name: driftHookName, Event: "pre_user_prompt", Handler: &struct {
			Bash string `json:"bash"`
		}{Bash: bash}},
		{Name: baselineHookName, Event: "session_start", Handler: &struct {
			Bash string `json:"bash"`
		}{Bash: bash}},
	}
}

// HookRegistrationFinding checks the default user configuration directory.
// The returned message deliberately contains no values read from hooks.json.
func HookRegistrationFinding(configDir, noticePath string) (managed, current bool, err error) {
	resolved, err := resolveConfigDir(configDir)
	if err != nil {
		return false, false, err
	}
	return HookRegistrationStatus(resolved, noticePath)
}
