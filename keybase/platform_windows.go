// Copyright 2016 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package keybase

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/osext"
	"github.com/keybase/go-updater"
	"github.com/keybase/go-updater/command"
	"github.com/keybase/go-updater/util"
	"golang.org/x/sys/windows/registry"
)

func (c config) destinationPath() string {
	pathName, err := osext.Executable()
	if err != nil {
		c.log.Warningf("Error trying to determine our executable path: %s", err)
		return ""
	}
	dir, _ := filepath.Split(pathName)
	return dir
}

// Dir returns where to store config and log files
func Dir(appName string) (string, error) {
	dir := os.Getenv("APPDATA")
	if dir == "" {
		return "", fmt.Errorf("No APPDATA env set")
	}
	if appName == "" {
		return "", fmt.Errorf("No app name for dir")
	}
	return filepath.Join(dir, appName), nil
}

// LogDir is where to log
func LogDir(appName string) (string, error) {
	return Dir(appName)
}

func (c config) osVersion() string {
	result, err := command.Exec("cmd", []string{"/c", "ver"}, 5*time.Second, c.log)
	if err != nil {
		c.log.Warningf("Error trying to determine OS version: %s (%s)", err, result.CombinedOutput())
		return ""
	}
	return strings.TrimSpace(result.Stdout.String())
}

func (c config) notifyProgram() string {
	// No notify program for Windows
	return ""
}

func (c *context) BeforeUpdatePrompt(update updater.Update, options updater.UpdateOptions) error {
	// This is the check whether to override an auto update. Unnecessary if
	// auto update is not on in the first place.
	_, auto := c.config.GetUpdateAuto()
	if auto && !c.config.GetUpdateAutoOverride() {
		Dokan86 := ""
		Dokan64 := ""
		for _, prop := range update.Props {
			if prop.Name == "DokanProductCodeX64" {
				Dokan64 = prop.Value
			} else if prop.Name == "DokanProductCodeX86" {
				Dokan86 = prop.Value
			}
		}
		if canBeSilent, _ := CheckCanBeSilent(Dokan64, Dokan86, c.log, CheckRegistryUninstallCode); !canBeSilent {
			c.config.SetUpdateAutoOverride(true)
		}
	}
	return nil
}

type regUninstallGetter func(string, Log) bool

// CheckCanBeSilent - Interrogate the incoming installer for its driver's
// uninstall codes. It turns out a Wix bundle .exe supports "/layout", which
// among other things generates a log of what it would do. We can parse this
// for Dokan product code variables, which will be in the registry if the same
// version is already present.
func CheckCanBeSilent(dokanCodeX86 string, dokanCodeX64 string, log Log, regFunc regUninstallGetter) (bool, error) {
	codeFound := false
	var err error

	// Also look in the registry whether a reboot is pending from a previous installer.
	// Not finding the key is not an error.
	if rebootPending, err := checkRebootPending(false, log); rebootPending && err == nil {
		return false, nil
	}

	if rebootPending, err := checkRebootPending(true, log); rebootPending && err == nil {
		return false, nil
	}

	if regFunc(dokanCodeX86, log) || regFunc(dokanCodeX64, log) {
		codeFound = true
	}

	log.Infof("CheckCanBeSilent: returning %v", codeFound)
	return codeFound, err
}

// CheckRegistryUninstallCode is exported so a little standalone
// test utility can use it
func CheckRegistryUninstallCode(productID string, log Log) bool {
	log.Infof("CheckCanBeSilent: Searching registry for %s", productID)
	if productID == "" {
		log.Info("CheckCanBeSilent: Empty product ID, returning false")
		return false
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\`+productID, registry.QUERY_VALUE|registry.WOW64_64KEY)
	defer util.Close(k)
	if err == nil {
		log.Infof("CheckCanBeSilent: Found %s", productID)
		return true
	}
	return false
}

// Read all the runonce subkeys and find the one with "Keybase" in the name.
func checkRebootPending(wow64 bool, log Log) (bool, error) {
	var access uint32 = registry.ENUMERATE_SUB_KEYS | registry.QUERY_VALUE
	if wow64 {
		access = access | registry.WOW64_32KEY
	}

	k, err := registry.OpenKey(registry.CURRENT_USER, "SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\RunOnce", access)
	if err != nil {
		log.Errorf("Error opening RunOnce subkeys: %s", err)
		return false, err
	}
	defer util.Close(k)

	params, err := k.ReadValueNames(0)
	if err != nil {
		log.Errorf("Can't ReadSubKeyNames %#v", err)
		return false, err
	}

	for _, param := range params {
		val, _, err := k.GetStringValue(param)

		if err != nil {
			log.Warningf("Error getting string value for %s: %s", param, err)
			continue
		}
		if strings.Contains(val, "Keybase") {
			return true, nil
		}
	}

	// Check for a Dokan reboot pending
	k2, err := registry.OpenKey(registry.LOCAL_MACHINE, "SYSTEM\\CurrentControlSet\\Control\\Session Manager", access)
	if err != nil {
		log.Errorf("Error opening Session Manager key: %s", err)
		return false, err
	}
	defer util.Close(k2)
	vals, _, err := k2.GetStringsValue("PendingFileRenameOperations")
	if err != nil {
		// This is normal if no reboot is pending
		log.Errorf("Error getting PendingFileRenameOperations: %s", err)
	} else {
		for _, val := range vals {
			if strings.Contains(strings.ToLower(val), "dokan") {
				log.Info("Found Dokan reboot pending")
				return true, nil
			}
		}
	}

	return false, nil
}

func (c config) promptProgram() (command.Program, error) {
	destinationPath := c.destinationPath()
	if destinationPath == "" {
		return command.Program{}, fmt.Errorf("No destination path")
	}

	return command.Program{
		Path: "mshta.exe",
		Args: []string{filepath.Join(destinationPath, "prompter", "prompter.hta")},
	}, nil
}

func (c context) UpdatePrompt(update updater.Update, options updater.UpdateOptions, promptOptions updater.UpdatePromptOptions) (*updater.UpdatePromptResponse, error) {
	promptProgram, err := c.config.promptProgram()
	if err != nil {
		return nil, err
	}

	if promptOptions.OutPath == "" {
		promptOptions.OutPath, err = util.WriteTempFile("updatePrompt", []byte{}, 0700)
		if err != nil {
			return nil, err
		}
		defer util.RemoveFileAtPath(promptOptions.OutPath)
	}

	promptJSONInput, err := c.promptInput(update, options, promptOptions)
	if err != nil {
		return nil, fmt.Errorf("Error generating input: %s", err)
	}

	_, err = command.Exec(promptProgram.Path, promptProgram.ArgsWith([]string{string(promptJSONInput)}), time.Hour, c.log)
	if err != nil {
		return nil, fmt.Errorf("Error running command: %s", err)
	}

	result, err := c.updaterPromptResultFromFile(promptOptions.OutPath)
	if err != nil {
		return nil, fmt.Errorf("Error reading result: %s", err)
	}
	return c.responseForResult(*result)
}

// updaterPromptResultFromFile gets the result from path decodes it
func (c context) updaterPromptResultFromFile(path string) (*updaterPromptInputResult, error) {
	resultRaw, err := util.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var result updaterPromptInputResult
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c context) PausedPrompt() bool {
	return false
}

func (c context) Apply(update updater.Update, options updater.UpdateOptions, tmpDir string) error {
	if update.Asset == nil || update.Asset.LocalPath == "" {
		return fmt.Errorf("No asset")
	}
	var args []string
	auto, _ := c.config.GetUpdateAuto()
	if auto && !c.config.GetUpdateAutoOverride() {
		args = append(args, "/quiet")
	}
	_, err := command.Exec(update.Asset.LocalPath, args, time.Hour, c.log)
	return err
}

func (c context) Restart() error {
	// Restart is handled by the installer
	return nil
}
