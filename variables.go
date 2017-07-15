package task

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/go-task/task/execext"

	"github.com/Masterminds/sprig"
)

var (
	// TaskvarsFilePath file containing additional variables
	TaskvarsFilePath = "Taskvars"
	// ErrMultilineResultCmd is returned when a command returns multiline result
	ErrMultilineResultCmd = errors.New("Got multiline result from command")
)

// Vars is a string[string] variables map
type Vars map[string]Var

// Var represents either a static or dynamic variable
type Var struct {
	Static string
	Sh     string
}

func (vs Vars) toStringMap() (m map[string]string) {
	m = make(map[string]string, len(vs))
	for k, v := range vs {
		m[k] = v.Static
	}
	return
}

var (
	// ErrCantUnmarshalVar is returned for invalid var YAML
	ErrCantUnmarshalVar = errors.New("task: can't unmarshal var value")
)

// UnmarshalYAML implements yaml.Unmarshaler interface
func (v *Var) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var str string
	if err := unmarshal(&str); err == nil {
		if strings.HasPrefix(str, "$") {
			v.Sh = strings.TrimPrefix(str, "$")
		} else {
			v.Static = str
		}
		return nil
	}

	var sh struct {
		Sh string
	}
	if err := unmarshal(&sh); err == nil {
		v.Sh = sh.Sh
		return nil
	}
	return ErrCantUnmarshalVar
}

var (
	templateFuncs template.FuncMap
)

func init() {
	taskFuncs := template.FuncMap{
		"OS":   func() string { return runtime.GOOS },
		"ARCH": func() string { return runtime.GOARCH },
		// historical reasons
		"IsSH": func() bool { return true },
		"FromSlash": func(path string) string {
			return filepath.FromSlash(path)
		},
		"ToSlash": func(path string) string {
			return filepath.ToSlash(path)
		},
		"ExeExt": func() string {
			if runtime.GOOS == "windows" {
				return ".exe"
			}
			return ""
		},
	}

	templateFuncs = sprig.TxtFuncMap()
	for k, v := range taskFuncs {
		templateFuncs[k] = v
	}
}

// ReplaceVariables writes vars into initial string
func (e *Executor) ReplaceVariables(initial string, call Call) (string, error) {
	templ, err := template.New("").Funcs(templateFuncs).Parse(initial)
	if err != nil {
		return "", err
	}

	var b bytes.Buffer
	if err = templ.Execute(&b, call.Vars.toStringMap()); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ReplaceSliceVariables writes vars into initial string slice
func (e *Executor) ReplaceSliceVariables(initials []string, call Call) ([]string, error) {
	result := make([]string, len(initials))
	for i, s := range initials {
		var err error
		result[i], err = e.ReplaceVariables(s, call)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (e *Executor) getVariables(call Call) (Vars, error) {
	t := e.Tasks[call.Task]

	result := make(Vars, len(t.Vars)+len(e.taskvars)+len(call.Vars))
	merge := func(vars Vars, runTemplate bool) error {
		for k, v := range vars {
			if runTemplate {
				var err error
				v.Static, err = e.ReplaceVariables(v.Static, call)
				if err != nil {
					return err
				}
				v.Sh, err = e.ReplaceVariables(v.Sh, call)
				if err != nil {
					return err
				}
			}

			v, err := e.handleDynamicVariableContent(v)
			if err != nil {
				return err
			}

			result[k] = Var{Static: v}
		}
		return nil
	}

	if err := merge(e.taskvars, false); err != nil {
		return nil, err
	}
	if err := merge(t.Vars, true); err != nil {
		return nil, err
	}
	if err := merge(getEnvironmentVariables(), false); err != nil {
		return nil, err
	}
	if err := merge(call.Vars, false); err != nil {
		return nil, err
	}

	return result, nil
}

// GetEnvironmentVariables returns environment variables as map
func getEnvironmentVariables() Vars {
	var (
		env = os.Environ()
		m   = make(Vars, len(env))
	)

	for _, e := range env {
		keyVal := strings.SplitN(e, "=", 2)
		key, val := keyVal[0], keyVal[1]
		m[key] = Var{Static: val}
	}
	return m
}

func (e *Executor) handleDynamicVariableContent(v Var) (string, error) {
	if v.Static != "" {
		return v.Static, nil
	}

	e.muDynamicCache.Lock()
	defer e.muDynamicCache.Unlock()
	if result, ok := e.dynamicCache[v.Sh]; ok {
		return result, nil
	}

	var stdout bytes.Buffer
	opts := &execext.RunCommandOptions{
		Command: v.Sh,
		Dir:     e.Dir,
		Stdout:  &stdout,
		Stderr:  e.Stderr,
	}
	if err := execext.RunCommand(opts); err != nil {
		return "", &dynamicVarError{cause: err, cmd: opts.Command}
	}

	result := strings.TrimSuffix(stdout.String(), "\n")
	if strings.ContainsRune(result, '\n') {
		return "", ErrMultilineResultCmd
	}

	result = strings.TrimSpace(result)
	e.verbosePrintfln(`task: dynamic variable: "%s", result: "%s"`, v.Sh, result)
	e.dynamicCache[v.Sh] = result
	return result, nil
}
