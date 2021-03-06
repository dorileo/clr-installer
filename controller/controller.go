// Copyright © 2018 Intel Corporation
//
// SPDX-License-Identifier: GPL-3.0-only

package controller

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/clearlinux/clr-installer/args"
	"github.com/clearlinux/clr-installer/cmd"
	"github.com/clearlinux/clr-installer/conf"
	"github.com/clearlinux/clr-installer/errors"
	"github.com/clearlinux/clr-installer/hostname"
	"github.com/clearlinux/clr-installer/keyboard"
	"github.com/clearlinux/clr-installer/language"
	"github.com/clearlinux/clr-installer/log"
	"github.com/clearlinux/clr-installer/model"
	"github.com/clearlinux/clr-installer/network"
	"github.com/clearlinux/clr-installer/progress"
	"github.com/clearlinux/clr-installer/storage"
	"github.com/clearlinux/clr-installer/swupd"
	"github.com/clearlinux/clr-installer/telemetry"
	"github.com/clearlinux/clr-installer/timezone"
	cuser "github.com/clearlinux/clr-installer/user"
	"github.com/clearlinux/clr-installer/utils"
)

var (
	// NetworkPassing is used to track if the latest network configuration
	// is passing; changes in proxy, etc.
	NetworkPassing bool
)

func sortMountPoint(bds []*storage.BlockDevice) []*storage.BlockDevice {
	sort.Slice(bds[:], func(i, j int) bool {
		return filepath.HasPrefix(bds[j].MountPoint, bds[i].MountPoint)
	})

	return bds
}

// Install is the main install controller, this is the entry point for a full
// installation
func Install(rootDir string, model *model.SystemInstall, options args.Args) error {
	var err error
	var version string
	var versionBuf []byte
	var prg progress.Progress

	vars := map[string]string{
		"chrootDir": rootDir,
	}

	for k, v := range model.Environment {
		vars[k] = v
	}

	// First verify we are running as 'root' user which is required
	// for most of the Installation commands
	if err = utils.VerifyRootUser(); err != nil {
		return err
	}

	if !options.StubImage {
		if err = applyHooks("pre-install", vars, model.PreInstall); err != nil {
			return err
		}

		if model.Telemetry.Enabled {
			if err = model.Telemetry.CreateLocalTelemetryConf(); err != nil {
				return err
			}
			if model.Telemetry.URL != "" {
				if err = model.Telemetry.UpdateLocalTelemetryServer(); err != nil {
					return err
				}
			}
			if err = model.Telemetry.RestartLocalTelemetryServer(); err != nil {
				return err
			}
		}
	}

	if model.Version == 0 {
		log.Info("Querying Clear Linux version")

		// in order to avoid issues raised by format bumps between installers image
		// version and the latest released we assume the installers host version
		// in other words we use the same version swupd is based on
		if versionBuf, err = ioutil.ReadFile("/usr/lib/os-release"); err != nil {
			return errors.Errorf("Read version file /usr/lib/os-release: %v", err)
		}
		versionExp := regexp.MustCompile(`VERSION_ID=([0-9][0-9]*)`)
		match := versionExp.FindSubmatch(versionBuf)

		if len(match) < 2 {
			return errors.Errorf("Version not found in /usr/lib/os-release")
		}

		version = string(match[1])
	} else {
		version = fmt.Sprintf("%d", model.Version)
	}

	log.Debug("Clear Linux version: %s", version)

	// do we have the minimum required to install a system?
	if err = model.Validate(); err != nil {
		return err
	}

	// Using MassInstaller (non-UI) the network will not have been checked yet
	if !NetworkPassing && !options.StubImage {
		if err = ConfigureNetwork(model); err != nil {
			return err
		}
	}

	expandMe := []*storage.BlockDevice{}
	detachMe := []string{}
	aliasMap := map[string]string{}

	// prepare image file, case the user has declared image alias then create
	// the image, setup the loop device, prepare the variable expansion
	for _, alias := range model.StorageAlias {
		var file string

		if alias.DeviceFile {
			continue
		}

		// create the image and add the alias name to the variable expansion list
		for _, tm := range model.TargetMedias {
			if tm.Name == fmt.Sprintf("${%s}", alias.Name) {
				if err = storage.MakeImage(tm, alias.File); err != nil {
					return err
				}

				expandMe = append(expandMe, tm)
			}
		}

		file, err = storage.SetupLoopDevice(alias.File)
		if err != nil {
			return errors.Wrap(err)
		}

		aliasMap[alias.Name] = filepath.Base(file)
		detachMe = append(detachMe, file)

		retry := 5

		// wait the loop device to be prepared and available with 5 retry attempts
		for {
			var ok bool

			if ok, err = utils.FileExists(file); err != nil {
				for _, file := range detachMe {
					storage.DetachLoopDevice(file)
				}

				return errors.Wrap(err)
			}

			if ok || retry == 0 {
				break
			}

			retry--
			time.Sleep(time.Second * 1)
		}
	}

	// defer detaching used loop devices
	defer func() {
		for _, file := range detachMe {
			storage.DetachLoopDevice(file)
		}
	}()

	// expand block device's name case we've detected image replacement cases
	for _, tm := range expandMe {
		tm.ExpandName(aliasMap)
	}

	mountPoints := []*storage.BlockDevice{}

	// prepare all the target block devices
	for _, curr := range model.TargetMedias {
		// based on the description given, write the partition table
		if err = curr.WritePartitionTable(); err != nil {
			return err
		}

		// prepare the blockdevice's partitions filesystem
		for _, ch := range curr.Children {
			msg := fmt.Sprintf("Writing %s file system to %s", ch.FsType, ch.Name)
			prg = progress.NewLoop(msg)
			log.Info(msg)
			if err = ch.MakeFs(); err != nil {
				return err
			}
			prg.Success()

			// if we have a mount point set it for future mounting
			if ch.MountPoint != "" {
				mountPoints = append(mountPoints, ch)
			}
		}
	}

	if options.StubImage {
		return nil
	}

	// mount all the prepared partitions
	for _, curr := range sortMountPoint(mountPoints) {
		log.Info("Mounting: %s", curr.MountPoint)

		if err = curr.Mount(rootDir); err != nil {
			return err
		}
	}

	defer func() {
		log.Info("Umounting rootDir: %s", rootDir)
		if storage.UmountAll() != nil {
			log.Warning("Failed to umount volumes")
			return
		}

		log.Info("Removing rootDir: %s", rootDir)
		if err = os.RemoveAll(rootDir); err != nil {
			log.Warning("Failed to remove rootDir: %s", rootDir)
		}
	}()

	err = storage.MountMetaFs(rootDir)
	if err != nil {
		return err
	}

	if model.Telemetry.Enabled {
		model.AddBundle(telemetry.RequiredBundle)
	}

	if len(model.Users) > 0 {
		model.AddBundle(cuser.RequiredBundle)
	}

	if model.Timezone.Code != timezone.DefaultTimezone {
		model.AddBundle(timezone.RequiredBundle)
	}

	if model.Keyboard.Code != keyboard.DefaultKeyboard {
		model.AddBundle(keyboard.RequiredBundle)
	}

	if model.Language.Code != language.DefaultLanguage {
		model.AddBundle(language.RequiredBundle)
	}

	if model.KernelArguments != nil && len(model.KernelArguments.Add) > 0 {
		cmdlineDir := filepath.Join(rootDir, "etc", "kernel")
		cmdlineFile := filepath.Join(cmdlineDir, "cmdline")
		cmdline := strings.Join(model.KernelArguments.Add, " ")

		if err = utils.MkdirAll(cmdlineDir, 0755); err != nil {
			return err
		}

		if err = ioutil.WriteFile(cmdlineFile, []byte(cmdline), 0644); err != nil {
			return err
		}
	}

	if model.KernelArguments != nil && len(model.KernelArguments.Remove) > 0 {
		cmdlineDir := filepath.Join(rootDir, "etc", "kernel", "cmdline-removal.d")
		cmdlineFile := filepath.Join(cmdlineDir, "clr-installer.conf")
		cmdline := strings.Join(model.KernelArguments.Remove, " ")

		if err = utils.MkdirAll(cmdlineDir, 0755); err != nil {
			return err
		}

		if err = ioutil.WriteFile(cmdlineFile, []byte(cmdline), 0644); err != nil {
			return err
		}
	}

	if prg, err = contentInstall(rootDir, version, model, options); err != nil {
		prg.Failure()
		return err
	}

	if err = configureTimezone(rootDir, model); err != nil {
		// Just log the error, not setting the timezone is not reason to fail the install
		log.Error("Error setting timezone: %v", err)
	}

	if err = configureKeyboard(rootDir, model); err != nil {
		// Just log the error, not setting the keyboard is not reason to fail the install
		log.Error("Error setting keyboard: %v", err)
	}

	if err = configureLanguage(rootDir, model); err != nil {
		// Just log the error, not setting the language is not reason to fail the install
		log.Error("Error setting language locale: %v", err)
	}

	if err = cuser.Apply(rootDir, model.Users); err != nil {
		return err
	}

	if model.Hostname != "" {
		if err = hostname.SetTargetHostname(rootDir, model.Hostname); err != nil {
			return err
		}
	}

	if model.Telemetry.URL != "" {
		if err = model.Telemetry.CreateTelemetryConf(rootDir); err != nil {
			return err
		}
	}

	if err = applyHooks("post-install", vars, model.PostInstall); err != nil {
		return err
	}

	msg := "Saving the installation results"
	prg = progress.NewLoop(msg)
	log.Info(msg)
	if err = saveInstallResults(rootDir, model); err != nil {
		log.ErrorError(err)
	}
	prg.Success()

	return nil
}

func applyHooks(name string, vars map[string]string, hooks []*model.InstallHook) error {
	msg := fmt.Sprintf("Running %s hooks", name)
	prg := progress.MultiStep(len(hooks), msg)
	log.Info(msg)

	for idx, curr := range hooks {
		if err := runInstallHook(vars, curr); err != nil {
			prg.Failure()
			return err
		}
		prg.Partial(idx)
	}

	prg.Success()
	return nil
}

func runInstallHook(vars map[string]string, hook *model.InstallHook) error {
	args := []string{}
	vars["chrooted"] = "0"

	if hook.Chroot {
		args = append(args, []string{"chroot", vars["chrootDir"]}...)
		vars["chrooted"] = "1"
	}

	exec := utils.ExpandVariables(vars, hook.Cmd)
	args = append(args, []string{"bash", "-c", exec}...)

	if err := cmd.RunAndLogWithEnv(vars, args...); err != nil {
		return errors.Wrap(err)
	}

	return nil
}

// use the current host's version to bootstrap the sysroot, then update to the
// latest one and start adding new bundles
// for the bootstrap we use the hosts's swupd and the following operations are
// executed using the target swupd
func contentInstall(rootDir string, version string, model *model.SystemInstall, options args.Args) (progress.Progress, error) {

	sw := swupd.New(rootDir, options)

	msg := "Installing the base system"
	prg := progress.NewLoop(msg)
	log.Info(msg)
	if err := sw.Verify(version, model.SwupdMirror); err != nil {
		return prg, err
	}

	if model.AutoUpdate {
		if err := sw.Update(); err != nil {
			return prg, err
		}
	} else {
		log.Info("Skipping initial swupd update due to Disabling of Auto Update")
		log.Info("Disabling 'swupd autoupdate' on Target")
		if err := sw.DisableUpdate(); err != nil {
			log.Warning("Disabling 'swupd autoupdate' on Target FAILED!")
			return prg, err
		}
	}
	prg.Success()

	bundles := model.Bundles

	if model.Kernel.Bundle != "none" {
		bundles = append(bundles, model.Kernel.Bundle)
	}

	for _, bundle := range bundles {
		// swupd will fail (return exit code 18) if we try to "re-install" a bundle
		// already installed - with that we need to prevent doing bundle-add for bundles
		// previously installed by verify operation
		if swupd.IsCoreBundle(bundle) {
			log.Debug("Bundle %s was already installed with the core bundles, skipping", bundle)
			continue
		}

		msg = fmt.Sprintf("Installing bundle: %s", bundle)
		prg = progress.NewLoop(msg)
		log.Info(msg)
		if err := sw.BundleAdd(bundle); err != nil {
			// Attempt to continue the installation for non-core bundles
			if errLog := model.Telemetry.LogRecord("swupd", 2, "Failed to install bundle: "+bundle); errLog != nil {
				log.Error("Failed to log Telemetry record for failed bundled: " + bundle)
			}
			log.Error("Failed to install bundle: %s", bundle)
			prg.Failure()
		} else {
			prg.Success()
		}
	}

	msg = "Installing boot loader"
	prg = progress.NewLoop(msg)
	log.Info(msg)
	args := []string{
		fmt.Sprintf("%s/usr/bin/clr-boot-manager", rootDir),
		"update",
		fmt.Sprintf("--path=%s", rootDir),
	}

	err := cmd.RunAndLog(args...)
	if err != nil {
		return prg, errors.Wrap(err)
	}
	prg.Success()

	return nil, nil
}

// ConfigureNetwork applies the model/configured network interfaces
func ConfigureNetwork(model *model.SystemInstall) error {
	prg, err := configureNetwork(model)
	if err != nil {
		prg.Success()
		NetworkPassing = false
		return err
	}

	NetworkPassing = true

	return nil
}

func configureNetwork(model *model.SystemInstall) (progress.Progress, error) {
	cmd.SetHTTPSProxy(model.HTTPSProxy)

	if len(model.NetworkInterfaces) > 0 {
		msg := "Applying network settings"
		prg := progress.NewLoop(msg)
		log.Info(msg)
		if err := network.Apply("/", model.NetworkInterfaces); err != nil {
			return prg, err
		}
		prg.Success()

		msg = "Restarting network interfaces"
		prg = progress.NewLoop(msg)
		log.Info(msg)
		if err := network.Restart(); err != nil {
			return prg, err
		}
		prg.Success()
	}

	msg := "Testing connectivity"
	prg := progress.NewLoop(msg)
	ok := false

	// 3 attempts to test connectivity
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Second)

		log.Info(msg)
		if err := network.VerifyConnectivity(); err == nil {
			ok = true
			break
		}
	}

	if !ok {
		return prg, errors.Errorf("Failed, network is not working.")
	}

	prg.Success()

	return nil, nil
}

// configureTimezone applies the model/configured Timezone to the target
func configureTimezone(rootDir string, model *model.SystemInstall) error {
	if model.Timezone.Code == timezone.DefaultTimezone {
		log.Debug("Skipping setting timezone " + model.Timezone.Code)
		return nil
	}

	msg := "Setting Timezone to " + model.Timezone.Code
	prg := progress.NewLoop(msg)
	log.Info(msg)

	err := timezone.SetTargetTimezone(rootDir, model.Timezone.Code)
	if err != nil {
		prg.Failure()
		return err
	}
	prg.Success()

	return nil
}

// configureKeyboard applies the model/configured keyboard to the target
func configureKeyboard(rootDir string, model *model.SystemInstall) error {
	if model.Keyboard.Code == keyboard.DefaultKeyboard {
		log.Debug("Skipping setting keyboard " + model.Keyboard.Code)
		return nil
	}

	msg := "Setting Keyboard to " + model.Keyboard.Code
	prg := progress.NewLoop(msg)
	log.Info(msg)

	err := keyboard.SetTargetKeyboard(rootDir, model.Keyboard.Code)
	if err != nil {
		prg.Failure()
		return err
	}
	prg.Success()

	return nil
}

// configureLanguage applies the model/configured language to the target
func configureLanguage(rootDir string, model *model.SystemInstall) error {
	if model.Language.Code == language.DefaultLanguage {
		log.Debug("Skipping setting language locale " + model.Language.Code)
		return nil
	}

	msg := "Setting Language locale to " + model.Language.Code
	prg := progress.NewLoop(msg)
	log.Info(msg)

	err := language.SetTargetLanguage(rootDir, model.Language.Code)
	if err != nil {
		prg.Failure()
		return err
	}
	prg.Success()

	return nil
}

// saveInstallResults saves the results of the installation process
// onto the target media
func saveInstallResults(rootDir string, md *model.SystemInstall) error {
	var err error
	errMsgs := []string{}

	// Log a sanitized YAML file with Telemetry
	var cleanModel model.SystemInstall
	// Marshal current into bytes
	confBytes, bytesErr := yaml.Marshal(md)
	if bytesErr != nil {
		log.Error("Failed to generate a copy of YAML data (%v)", bytesErr)
		errMsgs = append(errMsgs, "Failed to generate YAML file")
	}
	// Unmarshal into a copy
	if yamlErr := yaml.Unmarshal(confBytes, &cleanModel); yamlErr != nil {
		errMsgs = append(errMsgs, "Failed to duplicate YAML file")
	}
	// Sanitize the config data to remove any potential
	// Personal Information from the data set
	cleanModel.Users = nil      // Remove User Info
	cleanModel.Hostname = ""    // Remove user defined hostname
	cleanModel.HTTPSProxy = ""  // Remove user defined Proxy
	cleanModel.SwupdMirror = "" // Remove user defined Swupd Mirror

	var payload string
	confBytes, bytesErr = yaml.Marshal(cleanModel)
	if bytesErr != nil {
		log.Error("Failed to generate a sanitized data (%v)", bytesErr)
		errMsgs = append(errMsgs, "Failed to generate a sanitized YAML file")
		payload = strings.Join(errMsgs, ";")
	} else {
		payload = string(confBytes[:])
	}

	if errLog := md.Telemetry.LogRecord("success", 1, payload); errLog != nil {
		log.Error("Failed to log Telemetry success record")
	}

	if md.PostArchive {
		log.Info("Saving Installation results to %s", rootDir)

		saveDir := filepath.Join(rootDir, "root")
		if err = utils.MkdirAll(saveDir, 0755); err != nil {
			// Fallback in the unlikely case we can't use root's home
			saveDir = rootDir
		}

		confFile := filepath.Join(saveDir, conf.ConfigFile)

		if err := md.WriteFile(confFile); err != nil {
			log.Error("Failed to write YAML file (%v) %q", err, confFile)
			errMsgs = append(errMsgs, "Failed to write YAML file")
		}

		logFile := filepath.Join(saveDir, conf.LogFile)

		if err := log.ArchiveLogFile(logFile); err != nil {
			errMsgs = append(errMsgs, "Failed to archive log file")
		}

	} else {
		log.Info("Skipping archiving of Installation results")
	}

	// Give Telemetry a chance to send before we shutdown and copy
	time.Sleep(2 * time.Second)

	if err := md.Telemetry.StopLocalTelemetryServer(); err != nil {
		log.Warning("Failed to stop image Telemetry server")
		errMsgs = append(errMsgs, "Failed to stop image Telemetry server")
	}
	if err := md.Telemetry.CopyTelemetryRecords(rootDir); err != nil {
		log.Warning("Failed to copy image Telemetry data")
		errMsgs = append(errMsgs, "Failed to copy image Telemetry data")
	}

	if len(errMsgs) > 0 {
		return errors.Errorf("%s", strings.Join(errMsgs, ";"))
	}

	return nil
}
