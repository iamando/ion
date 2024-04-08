package project

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/kkqy/gokvpairs"
	"github.com/sst/ion/pkg/platform"
	"github.com/tailscale/hujson"
)

type step struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

type copyStep struct {
}

type npmStep struct {
	Package string `json:"package"`
	Version string `json:"version"`
	File    string `json:"file"`
	Dev     bool   `json:"dev"`
}

type patchStep struct {
	Patch json.RawMessage `json:"patch"`
	File  string          `json:"file"`
	Regex []struct {
		Find    string `json:"find"`
		Replace string `json:"replace"`
	} `json:"regex"`
}

type gitignoreStep struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type preset struct {
	Steps []step `json:"steps"`
}

var ErrConfigExists = fmt.Errorf("sst.config.ts already exists")
var ErrPackageJsonInvalid = fmt.Errorf("package.json is invalid")

func Create(templateName string, home string) error {
	gitignoreSteps := []gitignoreStep{
		{
			Name: "# sst",
			Path: ".sst",
		},
	}

	if _, err := os.Stat("sst.config.ts"); err == nil {
		return ErrConfigExists
	}

	currentDirectory, err := os.Getwd()
	if err != nil {
		return nil
	}
	directoryName := strings.ToLower(filepath.Base(currentDirectory))
	slog.Info("creating project", "name", directoryName)

	presetBytes, err := platform.Templates.ReadFile(filepath.Join("templates", templateName, "preset.json"))
	if err != nil {
		return fmt.Errorf("failed to read preset.json: %w", err)
	}
	var preset preset
	err = json.Unmarshal(presetBytes, &preset)
	if err != nil {
		return err
	}

	packageJsons := map[string]gokvpairs.KeyValuePairs[interface{}]{}

	for _, step := range preset.Steps {
		slog.Info("step", "type", step.Type)
		switch step.Type {
		case "npm":
			var npmStep npmStep
			err = json.Unmarshal(step.Properties, &npmStep)
			if err != nil {
				return err
			}
			slog.Info("installing npm package", "package", npmStep.Package, "version", npmStep.Version)
			packageJson := packageJsons[npmStep.File]
			if packageJson == nil {
				slog.Info("reading package.json", "file", npmStep.File)
				f, err := os.Open(npmStep.File)
				if err != nil {
					return err
				}
				err = json.NewDecoder(f).Decode(&packageJson)
				if err != nil {
					return err
				}
				packageJsons[npmStep.File] = packageJson
			}

			field := "dependencies"
			if npmStep.Dev {
				field = "devDependencies"
			}
			var target map[string]interface{}
			for _, item := range packageJson {
				if item.Key == field {
					target = item.Value.(map[string]interface{})
				}
			}
			if target == nil {
				target = map[string]interface{}{}
				packageJson = append(packageJson, gokvpairs.KeyValuePair[interface{}]{Key: field, Value: target})
				packageJsons[npmStep.File] = packageJson
			}

			version := npmStep.Version
			if version == "" {
				slog.Info("fetching latest version", "package", npmStep.Package)
				url := "https://registry.npmjs.org/" + npmStep.Package + "/latest"
				resp, err := http.Get(url)
				if err != nil {
					return err
				}
				defer resp.Body.Close()

				var data struct {
					Version string `json:"version"`
				}
				err = json.NewDecoder(resp.Body).Decode(&data)
				if err != nil {
					return err
				}
				slog.Info("latest version", "version", data.Version)
				version = data.Version
			}
			target[npmStep.Package] = version

		case "patch":
			var patchStep patchStep
			err = json.Unmarshal(step.Properties, &patchStep)
			if err != nil {
				return err
			}
			slog.Info("patching", "file", patchStep.File, "patch", patchStep.Patch)

			b, err := os.ReadFile(patchStep.File)
			if err != nil {
				if os.IsNotExist(err) {
					slog.Info("file does not exist, ignoring patch", "file", patchStep.File)
					continue
				}
				return err
			}

			value, err := hujson.Parse(b)
			if err != nil {
				return err
			}
			err = value.Patch(patchStep.Patch)
			if err != nil {
				return err
			}

			packed := string(value.Pack())
			for _, pattern := range patchStep.Regex {
				re := regexp.MustCompile(pattern.Find)
				packed = re.ReplaceAllString(packed, pattern.Replace)
			}

			file, err := os.Create(patchStep.File)
			if err != nil {
				return err
			}
			defer file.Close()
			file.WriteString(packed)

			break

		case "copy":
			templateFilesPath := filepath.Join("templates", templateName, "files")
			err = fs.WalkDir(platform.Templates, templateFilesPath, func(path string, d fs.DirEntry, err error) error {
				if d.IsDir() {
					// Create the directory if it doesn't exist
					dir := filepath.Join(".", strings.TrimPrefix(path, templateFilesPath))
					if dir == "" {
						return nil
					}
					err := os.MkdirAll(dir, 0755)
					if err != nil {
						return err
					}
					return nil
				}

				src, err := platform.Templates.ReadFile(path)
				if err != nil {
					return err
				}

				name := filepath.Join(".", strings.TrimPrefix(path, templateFilesPath))

				slog.Info("copying template", "path", path)
				tmpl, err := template.New(path).Parse(string(src))
				data := struct {
					App  string
					Home string
				}{
					App:  directoryName,
					Home: home,
				}

				if _, err := os.Stat(name); os.IsExist(err) {
					return nil
				}
				output, err := os.Create(name)
				if err != nil {
					return err
				}
				defer output.Close()

				err = tmpl.Execute(output, data)
				if err != nil {
					return err
				}

				return nil
			})
			if err != nil {
				return err
			}
			break

		case "gitignore":
			var gitignoreStep gitignoreStep
			err = json.Unmarshal(step.Properties, &gitignoreStep)
			if err != nil {
				return err
			}
			slog.Info("handling .gitignore", "section", gitignoreStep.Name)
			gitignoreSteps = append(gitignoreSteps, gitignoreStep)
		}
	}

	for file, content := range packageJsons {
		bytes, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			return err
		}
		err = os.WriteFile(file, bytes, 0666)
		if err != nil {
			return err
		}
	}

	// Update .gitignore
	gitignoreFilename := ".gitignore"
	file, err := os.OpenFile(gitignoreFilename, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer file.Close()
	bytes, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	content := string(bytes)

	for _, step := range gitignoreSteps {
		if !strings.Contains(content, step.Path) {
			if content != "" && !strings.HasSuffix(content, "\n") {
				file.WriteString("\n")
			}
			_, err := file.WriteString("\n" + step.Name + "\n" + step.Path + "\n")
			if err != nil {
				return err
			}
		}
	}

	return nil
}
