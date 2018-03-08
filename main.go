package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/bitrise-io/go-utils/colorstring"
	"github.com/bitrise-io/go-utils/command"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/ziputil"
	"github.com/bitrise-tools/go-steputils/input"
	"github.com/bitrise-tools/go-steputils/tools"
	ver "github.com/hashicorp/go-version"
	shellquote "github.com/kballard/go-shellquote"
	"github.com/pkg/errors"
)

const (
	ipaPathEnvKey = "BITRISE_IPA_PATH"

	appZipPathEnvKey = "BITRISE_APP_PATH"
	appDirPathEnvKey = "BITRISE_APP_DIR_PATH"

	dsymDirPathEnvKey = "BITRISE_DSYM_DIR_PATH"
	dsymZipPathEnvKey = "BITRISE_DSYM_PATH"

	apkPathEnvKey     = "BITRISE_APK_PATH"
	apkPathListEnvKey = "BITRISE_APK_PATH_LIST"
)

// ConfigsModel ...
type ConfigsModel struct {
	Platform      string
	Configuration string
	Target        string
	BuildConfig   string
	Options       string

	Username string
	Password string

	IonicVersion          string
	CordovaVersion        string
	CordovaIosVersion     string
	CordovaAndroidVersion string

	WorkDir   string
	DeployDir string
}

func createConfigsModelFromEnvs() ConfigsModel {
	return ConfigsModel{
		Platform:      os.Getenv("platform"),
		Configuration: os.Getenv("configuration"),
		Target:        os.Getenv("target"),
		BuildConfig:   os.Getenv("build_config"),
		Options:       os.Getenv("options"),

		Username: os.Getenv("ionic_username"),
		Password: os.Getenv("ionic_password"),

		IonicVersion:          os.Getenv("ionic_version"),
		CordovaVersion:        os.Getenv("cordova_version"),
		CordovaIosVersion:     os.Getenv("cordova_ios_version"),
		CordovaAndroidVersion: os.Getenv("cordova_android_version"),

		WorkDir:   os.Getenv("workdir"),
		DeployDir: os.Getenv("BITRISE_DEPLOY_DIR"),
	}
}

func (configs ConfigsModel) print() {
	log.Infof("Configs:")
	log.Printf("- Platform: %s", configs.Platform)
	log.Printf("- Configuration: %s", configs.Configuration)
	log.Printf("- Target: %s", configs.Target)
	log.Printf("- BuildConfig: %s", configs.BuildConfig)
	log.Printf("- Options: %s", configs.Options)

	log.Printf("- Username: %s", input.SecureInput(configs.Username))
	log.Printf("- Password: %s", input.SecureInput(configs.Password))

	log.Printf("- IonicVersion: %s", configs.IonicVersion)
	log.Printf("- CordovaVersion: %s", configs.CordovaVersion)
	log.Printf("- CordovaIosVersion: %s", configs.CordovaIosVersion)
	log.Printf("- CordovaAndroidVersion: %s", configs.CordovaAndroidVersion)

	log.Printf("- WorkDir: %s", configs.WorkDir)
	log.Printf("- DeployDir: %s", configs.DeployDir)
}

func (configs ConfigsModel) validate() error {
	if err := input.ValidateIfDirExists(configs.WorkDir); err != nil {
		return fmt.Errorf("WorkDir: %s", err)
	}

	if err := input.ValidateWithOptions(configs.Platform, "ios,android", "ios", "android"); err != nil {
		return fmt.Errorf("Platform: %s", err)
	}

	if err := input.ValidateIfNotEmpty(configs.Configuration); err != nil {
		return fmt.Errorf("Configuration: %s", err)
	}

	if err := input.ValidateIfNotEmpty(configs.Target); err != nil {
		return fmt.Errorf("Target: %s", err)
	}

	return nil
}

func moveAndExportOutputs(outputs []string, deployDir, envKey string, envListKey string) (string, []string, error) {
	outputToExport := ""
	APKPaths := make([]string, len(outputs))
	for x, output := range outputs {
		info, err := os.Lstat(output)
		if err != nil {
			return "", []string{}, err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			resolvedPth, err := os.Readlink(output)
			if err != nil {
				return "", []string{}, err
			}

			log.Warnf("Output: %s is a symlink to: %s", output, resolvedPth)

			if exist, err := pathutil.IsPathExists(resolvedPth); err != nil {
				return "", []string{}, err
			} else if !exist {
				return "", []string{}, fmt.Errorf("resolved path: %s does not exist", resolvedPth)
			}

			resolvedInfo, err := os.Lstat(resolvedPth)
			if err != nil {
				return "", []string{}, err
			}

			if resolvedInfo.Mode()&os.ModeSymlink != 0 {
				return "", []string{}, fmt.Errorf("resolved path: %s is still symlink", resolvedPth)
			}

			output = resolvedPth
			info = resolvedInfo
		}

		fileName := filepath.Base(output)
		destinationPth := filepath.Join(deployDir, fileName)

		if info.IsDir() {
			if err := command.CopyDir(output, destinationPth, false); err != nil {
				return "", []string{}, err
			}
		} else {
			if err := command.CopyFile(output, destinationPth); err != nil {
				return "", []string{}, err
			}
		}

		outputToExport = destinationPth
		APKPaths[x] = destinationPth
	}

	if outputToExport == "" {
		return "", []string{}, nil
	}

	if err := tools.ExportEnvironmentWithEnvman(envKey, outputToExport); err != nil {
		return "", []string{}, err
	}

	if err := tools.ExportEnvironmentWithEnvman(envListKey, strings.Join(APKPaths, "|")); err != nil {
		return "", []string{}, err
	}

	return outputToExport, APKPaths, nil
}

func ionicVersion() (*ver.Version, error) {
	cmd := command.New("ionic", "-v")
	cmd.SetStdin(strings.NewReader("Y"))
	out, err := cmd.RunAndReturnTrimmedCombinedOutput()
	if err != nil {
		return nil, err
	}

	// fix for ionic-cli intercative version output: `[1000D[K3.2.0`
	pattern := `(?P<version>\d+\.\d+\.\d+)`

	reader := strings.NewReader(out)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		if match := regexp.MustCompile(pattern).FindStringSubmatch(line); len(match) == 2 {
			versionStr := match[1]
			version, err := ver.NewVersion(versionStr)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse version: %s", versionStr)
			}
			return version, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return nil, fmt.Errorf("output: %s", out)
}

func cordovaVersion() (*ver.Version, error) {
	cmd := command.New("cordova", "-v")
	out, err := cmd.RunAndReturnTrimmedCombinedOutput()
	if err != nil {
		return nil, err
	}
	version, err := ver.NewVersion(out)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse version: %s", out)
	}
	return version, nil
}

func fail(format string, v ...interface{}) {
	log.Errorf(format, v...)
	os.Exit(1)
}

func getField(c ConfigsModel, field string) string {
	r := reflect.ValueOf(c)
	f := reflect.Indirect(r).FieldByName(field)
	return string(f.String())
}

func find(dir, extension string) ([]string, error) {
	matches := []string{}
	if walkErr := filepath.Walk(dir, func(pth string, f os.FileInfo, err error) error {
		if filepath.Ext(pth) == "."+extension {
			matches = append(matches, pth)
		}
		return err
	}); walkErr != nil {
		return nil, walkErr
	}
	return matches, nil
}

func main() {
	configs := createConfigsModelFromEnvs()

	fmt.Println()
	configs.print()

	if err := configs.validate(); err != nil {
		fail("Issue with input: %s", err)
	}

	// Change dir to working directory
	workDir, err := pathutil.AbsPath(configs.WorkDir)
	if err != nil {
		fail("Failed to expand WorkDir (%s), error: %s", configs.WorkDir, err)
	}

	currentDir, err := pathutil.CurrentWorkingDirectoryAbsolutePath()
	if err != nil {
		fail("Failed to get current directory, error: %s", err)
	}

	if workDir != currentDir {
		fmt.Println()
		log.Infof("Switch working directory to: %s", workDir)

		revokeFunc, err := pathutil.RevokableChangeDir(workDir)
		if err != nil {
			fail("Failed to change working directory, error: %s", err)
		}
		defer func() {
			fmt.Println()
			log.Infof("Reset working directory")
			if err := revokeFunc(); err != nil {
				fail("Failed to reset working directory, error: %s", err)
			}
		}()
	}

	// Update cordova and ionic version
	if configs.CordovaVersion != "" {
		fmt.Println()
		log.Infof("Updating cordova version to: %s", configs.CordovaVersion)

		if err := npmRemove(false, "cordova"); err != nil {
			fail("Failed to remove cordova, error: %s", err)
		}

		if err := npmInstall(true, "cordova@"+configs.CordovaVersion); err != nil {
			fail("Failed to install cordova, error: %s", err)
		}
	}

	if configs.IonicVersion != "" {
		fmt.Println()
		log.Infof("Updating ionic version to: %s", configs.IonicVersion)

		if err := npmRemove(false, "ionic"); err != nil {
			fail("Failed to remove ionic, error: %s", err)
		}

		if err := npmInstall(true, "ionic@"+configs.IonicVersion); err != nil {
			fail("Failed to install ionic, error: %s", err)
		}

		fmt.Println()
		log.Infof("Installing local ionic cli")
		if err := npmInstall(false, "ionic@"+configs.IonicVersion); err != nil {
			fail("command failed, error: %s", err)
		}
	}

	// Print cordova and ionic version
	cordovaVer, err := cordovaVersion()
	if err != nil {
		fail("Failed to get cordova version, error: %s", err)
	}

	fmt.Println()
	log.Printf("cordova version: %s", colorstring.Green(cordovaVer.String()))

	ionicVer, err := ionicVersion()
	if err != nil {
		fail("Failed to get ionic version, error: %s", err)
	}

	log.Printf("ionic version: %s", colorstring.Green(ionicVer.String()))

	// Ionic CLI plugins angular and cordova have been marked as deprecated for
	// version 3.8.0 and above.
	ionicVerConstraint, err := ver.NewConstraint("< 3.8.0")
	if err != nil {
		fail("Could not create version constraint for ionic: %s", err)
	}
	if ionicVerConstraint.Check(ionicVer) {
		fmt.Println()
		log.Infof("Installing cordova and angular plugins")
		if err := npmInstall(false, "@ionic/cli-plugin-ionic-angular@latest", "@ionic/cli-plugin-cordova@latest"); err != nil {
			fail("command failed, error: %s", err)
		}
	}

	//
	// ionic login
	if configs.Username != "" && configs.Password != "" {
		fmt.Println()
		log.Infof("Ionic login")

		cmdArgs := []string{"ionic", "login", configs.Username, configs.Password}
		cmd := command.New(cmdArgs[0], cmdArgs[1:]...)
		cmd.SetStdout(os.Stdout).SetStderr(os.Stderr).SetStdin(strings.NewReader("y"))

		log.Donef("$ ionic login *** ***")

		if err := cmd.Run(); err != nil {
			fail("command failed, error: %s", err)
		}
	}

	ionicMajorVersion := ionicVer.Segments()[0]

	platforms := []string{}
	if configs.Platform != "" {
		platformsSplit := strings.Split(configs.Platform, ",")
		for _, platform := range platformsSplit {
			platforms = append(platforms, strings.TrimSpace(platform))
		}
	}

	// ionic prepare
	fmt.Println()
	log.Infof("Building project")

	if false {
		// platform rm
		for _, platform := range platforms {
			cmdArgs := []string{"ionic"}
			if ionicMajorVersion > 2 {
				cmdArgs = append(cmdArgs, "cordova")
			}

			cmdArgs = append(cmdArgs, "platform", "rm")

			cmdArgs = append(cmdArgs, platform)

			cmd := command.New(cmdArgs[0], cmdArgs[1:]...)
			cmd.SetStdout(os.Stdout).SetStderr(os.Stderr).SetStdin(strings.NewReader("y"))

			log.Donef("$ %s", cmd.PrintableCommandArgs())

			if err := cmd.Run(); err != nil {
				fail("command failed, error: %s", err)
			}
		}
	}

	if false {
		// platform add
		for _, platform := range platforms {
			cmdArgs := []string{"ionic"}
			if ionicMajorVersion > 2 {
				cmdArgs = append(cmdArgs, "cordova")
			}

			cmdArgs = append(cmdArgs, "platform", "add")

			platformVersion := platform
			pv := getField(configs, "Cordova"+strings.Title(platform)+"Version")
			if pv == "master" {
				platformVersion = "https://github.com/apache/cordova-" + platform + ".git"
			} else if pv != "" {
				platformVersion = platform + "@" + pv
			}

			cmdArgs = append(cmdArgs, platformVersion)

			cmd := command.New(cmdArgs[0], cmdArgs[1:]...)
			cmd.SetStdout(os.Stdout).SetStderr(os.Stderr).SetStdin(strings.NewReader("y"))

			log.Donef("$ %s", cmd.PrintableCommandArgs())

			if err := cmd.Run(); err != nil {
				fail("command failed, error: %s", err)
			}
		}
	}

	{
		// prepare
		options := []string{}
		if configs.Options != "" {
			opts, err := shellquote.Split(configs.Options)
			if err != nil {
				fail("Failed to shell split Options (%s), error: %s", configs.Options, err)
			}
			options = opts
		}

		cmdArgs := []string{"ionic"}
		if ionicMajorVersion > 2 {
			cmdArgs = append(cmdArgs, "cordova")
		}

		cmdArgs = append(cmdArgs, "prepare")

		if configs.BuildConfig != "" {
			cmdArgs = append(cmdArgs, "--buildConfig", configs.BuildConfig)
		}

		cmdArgs = append(cmdArgs, options...)
		cmdArgs = append(cmdArgs)

		cmd := command.New(cmdArgs[0], cmdArgs[1:]...)
		cmd.SetStdout(os.Stdout).SetStderr(os.Stderr).SetStdin(strings.NewReader("y"))

		log.Donef("$ %s", cmd.PrintableCommandArgs())

		if err := cmd.Run(); err != nil {
			fail("command failed, error: %s", err)
		}
	}

	{
		// build
		for _, platform := range platforms {
			cmdArgs := []string{"cordova", "build"}

			if configs.Configuration != "" {
				cmdArgs = append(cmdArgs, "--"+configs.Configuration)
			}

			if configs.Target != "" {
				cmdArgs = append(cmdArgs, "--"+configs.Target)
			}

			cmdArgs = append(cmdArgs, platform)

			if configs.BuildConfig != "" {
				cmdArgs = append(cmdArgs, "--buildConfig", configs.BuildConfig)
			}

			cmd := command.New(cmdArgs[0], cmdArgs[1:]...)
			cmd.SetStdout(os.Stdout).SetStderr(os.Stderr).SetStdin(strings.NewReader("y"))

			log.Donef("$ %s", cmd.PrintableCommandArgs())

			if err := cmd.Run(); err != nil {
				fail("command failed, error: %s", err)
			}
		}
	}

	// collect outputs

	iosOutputDirExist := false
	iosOutputDir := filepath.Join(workDir, "platforms", "ios", "build", configs.Target)
	if exist, err := pathutil.IsDirExists(iosOutputDir); err != nil {
		fail("Failed to check if dir (%s) exist, error: %s", iosOutputDir, err)
	} else if exist {
		iosOutputDirExist = true

		fmt.Println()
		log.Infof("Collecting ios outputs")

		// ipa
		ipas, err := find(iosOutputDir, "ipa")
		if err != nil {
			fail("Failed to find ipas in dir (%s), error: %s", iosOutputDir, err)
		}

		if len(ipas) > 0 {
			if exportedPth, _, err := moveAndExportOutputs(ipas, configs.DeployDir, ipaPathEnvKey, apkPathListEnvKey); err != nil {
				fail("Failed to export ipas, error: %s", err)
			} else if exportedPth != "" {
				log.Donef("The ipa path is now available in the Environment Variable: %s (value: %s)", ipaPathEnvKey, exportedPth)
			}
		}
		// ---

		// dsym
		dsyms, err := find(iosOutputDir, "dSYM")
		if err != nil {
			fail("Failed to find dSYMs in dir (%s), error: %s", iosOutputDir, err)
		}

		if len(dsyms) > 0 {
			if exportedPth, _, err := moveAndExportOutputs(dsyms, configs.DeployDir, dsymDirPathEnvKey, apkPathListEnvKey); err != nil {
				fail("Failed to export dsyms, error: %s", err)
			} else if exportedPth != "" {
				log.Donef("The dsym dir path is now available in the Environment Variable: %s (value: %s)", dsymDirPathEnvKey, exportedPth)

				zippedExportedPth := exportedPth + ".zip"
				if err := ziputil.ZipDir(exportedPth, zippedExportedPth, false); err != nil {
					fail("Failed to zip dsym dir (%s), error: %s", exportedPth, err)
				}

				if err := tools.ExportEnvironmentWithEnvman(dsymZipPathEnvKey, zippedExportedPth); err != nil {
					fail("Failed to export dsym.zip (%s), error: %s", zippedExportedPth, err)
				}

				log.Donef("The dsym.zip path is now available in the Environment Variable: %s (value: %s)", dsymZipPathEnvKey, zippedExportedPth)
			}
		}
		// --

		// app
		apps, err := find(iosOutputDir, "app")
		if err != nil {
			fail("Failed to find apps in dir (%s), error: %s", iosOutputDir, err)
		}

		if len(apps) > 0 {
			if exportedPth, _, err := moveAndExportOutputs(apps, configs.DeployDir, appDirPathEnvKey, apkPathListEnvKey); err != nil {
				log.Warnf("Failed to export apps, error: %s", err)
			} else if exportedPth != "" {
				log.Donef("The app dir path is now available in the Environment Variable: %s (value: %s)", appDirPathEnvKey, exportedPth)

				zippedExportedPth := exportedPth + ".zip"
				if err := ziputil.ZipDir(exportedPth, zippedExportedPth, false); err != nil {
					fail("Failed to zip app dir (%s), error: %s", exportedPth, err)
				}

				if err := tools.ExportEnvironmentWithEnvman(appZipPathEnvKey, zippedExportedPth); err != nil {
					fail("Failed to export app.zip (%s), error: %s", zippedExportedPth, err)
				}

				log.Donef("The app.zip path is now available in the Environment Variable: %s (value: %s)", appZipPathEnvKey, zippedExportedPth)
			}
		}
		// ---
	}

	androidOutputDirExist := false
	androidOutputDir := filepath.Join(workDir, "platforms", "android", "build", "outputs", "apk")
	if exist, err := pathutil.IsDirExists(androidOutputDir); err != nil {
		fail("Failed to check if dir (%s) exist, error: %s", androidOutputDir, err)
	} else if exist {
		androidOutputDirExist = true

		fmt.Println()
		log.Infof("Collecting android outputs")

		apks, err := find(androidOutputDir, "apk")
		if err != nil {
			fail("Failed to find apks in dir (%s), error: %s", androidOutputDir, err)
		}

		if len(apks) > 0 {
			if exportedPth, exportedPaths, err := moveAndExportOutputs(apks, configs.DeployDir, apkPathEnvKey, apkPathListEnvKey); err != nil {
				fail("Failed to export apks, error: %s", err)
			} else if exportedPth != "" {
				log.Donef("The apk path is now available in the Environment Variable: %s (value: %s)", apkPathEnvKey, exportedPth)
				if len(exportedPaths) > 0 {
					log.Donef("The apk paths are now available in the Environment Variable: %s (value: %s)", apkPathListEnvKey, strings.Join(exportedPaths, "|"))
				}
			}
		}
	}

	if !iosOutputDirExist && !androidOutputDirExist {
		log.Warnf("No ios nor android platform's output dir exist")
		fail("No output generated")
	}
}
