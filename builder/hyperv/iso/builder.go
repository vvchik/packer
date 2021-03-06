// Copyright (c) Microsoft Open Technologies, Inc.
// All Rights Reserved.
// Licensed under the Apache License, Version 2.0.
// See License.txt in the project root for license information.
package iso

import (
	"errors"
	"fmt"
	"github.com/mitchellh/multistep"
	hypervcommon "github.com/mitchellh/packer/builder/hyperv/common"
	"github.com/mitchellh/packer/common"
	"github.com/mitchellh/packer/helper/communicator"
	"github.com/mitchellh/packer/helper/config"
	"github.com/mitchellh/packer/packer"
	powershell "github.com/mitchellh/packer/powershell"
	"github.com/mitchellh/packer/powershell/hyperv"
	"github.com/mitchellh/packer/template/interpolate"
	"log"
	"strings"
	"time"
)

const (
	DefaultDiskSize = 40000        // ~40GB
	MinDiskSize     = 10 * 1024    // 10GB
	MaxDiskSize     = 65536 * 1024 // 64TB

	DefaultRamSize = 1024  // 1GB
	MinRamSize     = 512   // 512MB
	MaxRamSize     = 32768 // 32GB

	LowRam = 512 // 512MB

	DefaultUsername = "vagrant"
	DefaultPassword = "vagrant"
)

// Builder implements packer.Builder and builds the actual Hyperv
// images.
type Builder struct {
	config Config
	runner multistep.Runner
}

type Config struct {
	common.PackerConfig         `mapstructure:",squash"`
	hypervcommon.FloppyConfig   `mapstructure:",squash"`
	hypervcommon.OutputConfig   `mapstructure:",squash"`
	hypervcommon.SSHConfig      `mapstructure:",squash"`
	hypervcommon.RunConfig      `mapstructure:",squash"`
	hypervcommon.ShutdownConfig `mapstructure:",squash"`

	// The size, in megabytes, of the hard disk to create for the VM.
	// By default, this is 130048 (about 127 GB).
	DiskSize uint `mapstructure:"disk_size"`
	// The size, in megabytes, of the computer memory in the VM.
	// By default, this is 1024 (about 1 GB).
	RamSizeMB uint `mapstructure:"ram_size_mb"`
	// A list of files to place onto a floppy disk that is attached when the
	// VM is booted. This is most useful for unattended Windows installs,
	// which look for an Autounattend.xml file on removable media. By default,
	// no floppy will be attached. All files listed in this setting get
	// placed into the root directory of the floppy and the floppy is attached
	// as the first floppy device. Currently, no support exists for creating
	// sub-directories on the floppy. Wildcard characters (*, ?, and [])
	// are allowed. Directory names are also allowed, which will add all
	// the files found in the directory to the floppy.
	FloppyFiles []string `mapstructure:"floppy_files"`
	//
	SecondaryDvdImages []string `mapstructure:"secondary_iso_images"`

	// The checksum for the OS ISO file. Because ISO files are so large,
	// this is required and Packer will verify it prior to booting a virtual
	// machine with the ISO attached. The type of the checksum is specified
	// with iso_checksum_type, documented below.
	ISOChecksum string `mapstructure:"iso_checksum"`
	// The type of the checksum specified in iso_checksum. Valid values are
	// "none", "md5", "sha1", "sha256", or "sha512" currently. While "none"
	// will skip checksumming, this is not recommended since ISO files are
	// generally large and corruption does happen from time to time.
	ISOChecksumType string `mapstructure:"iso_checksum_type"`
	// A URL to the ISO containing the installation image. This URL can be
	// either an HTTP URL or a file URL (or path to a file). If this is an
	// HTTP URL, Packer will download it and cache it between runs.
	RawSingleISOUrl string `mapstructure:"iso_url"`
	// Multiple URLs for the ISO to download. Packer will try these in order.
	// If anything goes wrong attempting to download or while downloading a
	// single URL, it will move on to the next. All URLs must point to the
	// same file (same checksum). By default this is empty and iso_url is
	// used. Only one of iso_url or iso_urls can be specified.
	ISOUrls []string `mapstructure:"iso_urls"`

	// This is the name of the new virtual machine.
	// By default this is "packer-BUILDNAME", where "BUILDNAME" is the name of the build.
	VMName string `mapstructure:"vm_name"`

	BootCommand      []string `mapstructure:"boot_command"`
	SwitchName       string   `mapstructure:"switch_name"`
	VlanId           string   `mapstructure:"vlan_id"`
	Cpu              uint     `mapstructure:"cpu"`
	Generation       uint     `mapstructure:"generation"`
	EnableSecureBoot bool     `mapstructure:"enable_secure_boot"`

	Communicator string `mapstructure:"communicator"`

	// The time in seconds to wait for the virtual machine to report an IP address.
	// This defaults to 120 seconds. This may have to be increased if your VM takes longer to boot.
	IPAddressTimeout time.Duration `mapstructure:"ip_address_timeout"`

	SSHWaitTimeout time.Duration

	SkipCompaction bool `mapstructure:"skip_compaction"`

	ctx interpolate.Context
}

// Prepare processes the build configuration parameters.
func (b *Builder) Prepare(raws ...interface{}) ([]string, error) {
	err := config.Decode(&b.config, &config.DecodeOpts{
		Interpolate: true,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"boot_command",
			},
		},
	}, raws...)
	if err != nil {
		return nil, err
	}

	// Accumulate any errors and warnings
	var errs *packer.MultiError
	errs = packer.MultiErrorAppend(errs, b.config.FloppyConfig.Prepare(&b.config.ctx)...)
	errs = packer.MultiErrorAppend(errs, b.config.RunConfig.Prepare(&b.config.ctx)...)
	errs = packer.MultiErrorAppend(errs, b.config.OutputConfig.Prepare(&b.config.ctx, &b.config.PackerConfig)...)
	errs = packer.MultiErrorAppend(errs, b.config.SSHConfig.Prepare(&b.config.ctx)...)
	errs = packer.MultiErrorAppend(errs, b.config.ShutdownConfig.Prepare(&b.config.ctx)...)
	warnings := make([]string, 0)

	err = b.checkDiskSize()
	if err != nil {
		errs = packer.MultiErrorAppend(errs, err)
	}

	err = b.checkRamSize()
	if err != nil {
		errs = packer.MultiErrorAppend(errs, err)
	}

	if b.config.VMName == "" {
		b.config.VMName = fmt.Sprintf("packer-%s", b.config.PackerBuildName)
	}

	log.Println(fmt.Sprintf("%s: %v", "VMName", b.config.VMName))

	if b.config.SwitchName == "" {
		// no switch name, try to get one attached to a online network adapter
		onlineSwitchName, err := hyperv.GetExternalOnlineVirtualSwitch()
		if onlineSwitchName == "" || err != nil {
			b.config.SwitchName = fmt.Sprintf("packer-%s", b.config.PackerBuildName)
		} else {
			b.config.SwitchName = onlineSwitchName
		}
	}

	if b.config.VlanId == "" {
		//	b.config.VlanId = "1724"
	}

	if b.config.Cpu < 1 {
		b.config.Cpu = 1
	}

	if b.config.Generation != 2 {
		b.config.Generation = 1
	}

	if b.config.Generation == 2 {
		if len(b.config.FloppyFiles) > 0 {
			err = errors.New("Generation 2 vms don't support floppy drives. Use ISO image instead.")
			errs = packer.MultiErrorAppend(errs, err)
		}
	}

	log.Println(fmt.Sprintf("Using switch %s", b.config.SwitchName))
	log.Println(fmt.Sprintf("%s: %v", "SwitchName", b.config.SwitchName))
	log.Println(fmt.Sprintf("Using vlan %s", b.config.VlanId))

	if b.config.Communicator == "" {
		b.config.Communicator = "ssh"
	} else if b.config.Communicator == "ssh" || b.config.Communicator == "winrm" {
		// good
	} else {
		err = errors.New("communicator must be either ssh or winrm")
		errs = packer.MultiErrorAppend(errs, err)
	}

	log.Println(fmt.Sprintf("%s: %v", "Communicator", b.config.Communicator))

	// Errors
	if b.config.ISOChecksumType == "" {
		errs = packer.MultiErrorAppend(
			errs, errors.New("The iso_checksum_type must be specified."))
	} else {
		b.config.ISOChecksumType = strings.ToLower(b.config.ISOChecksumType)
		if b.config.ISOChecksumType != "none" {
			if b.config.ISOChecksum == "" {
				errs = packer.MultiErrorAppend(
					errs, errors.New("Due to large file sizes, an iso_checksum is required"))
			} else {
				b.config.ISOChecksum = strings.ToLower(b.config.ISOChecksum)
			}

			if h := common.HashForType(b.config.ISOChecksumType); h == nil {
				errs = packer.MultiErrorAppend(
					errs,
					fmt.Errorf("Unsupported checksum type: %s", b.config.ISOChecksumType))
			}
		}
	}

	if b.config.RawSingleISOUrl == "" && len(b.config.ISOUrls) == 0 {
		errs = packer.MultiErrorAppend(
			errs, errors.New("One of iso_url or iso_urls must be specified."))
	} else if b.config.RawSingleISOUrl != "" && len(b.config.ISOUrls) > 0 {
		errs = packer.MultiErrorAppend(
			errs, errors.New("Only one of iso_url or iso_urls may be specified."))
	} else if b.config.RawSingleISOUrl != "" {
		b.config.ISOUrls = []string{b.config.RawSingleISOUrl}
	}

	for i, url := range b.config.ISOUrls {
		b.config.ISOUrls[i], err = common.DownloadableURL(url)
		if err != nil {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Failed to parse iso_url %d: %s", i+1, err))
		}
	}

	log.Println(fmt.Sprintf("%s: %v", "RawSingleISOUrl", b.config.RawSingleISOUrl))

	// Warnings
	if b.config.ISOChecksumType == "none" {
		warnings = append(warnings,
			"A checksum type of 'none' was specified. Since ISO files are so big,\n"+
				"a checksum is highly recommended.")
	}

	if b.config.ShutdownCommand == "" {
		warnings = append(warnings,
			"A shutdown_command was not specified. Without a shutdown command, Packer\n"+
				"will forcibly halt the virtual machine, which may result in data loss.")
	}

	warning := b.checkHostAvailableMemory()
	if warning != "" {
		warnings = appendWarnings(warnings, warning)
	}

	if errs != nil && len(errs.Errors) > 0 {
		return warnings, errs
	}

	return warnings, nil
}

// Run executes a Packer build and returns a packer.Artifact representing
// a Hyperv appliance.
func (b *Builder) Run(ui packer.Ui, hook packer.Hook, cache packer.Cache) (packer.Artifact, error) {
	// Create the driver that we'll use to communicate with Hyperv
	driver, err := hypervcommon.NewHypervPS4Driver()
	if err != nil {
		return nil, fmt.Errorf("Failed creating Hyper-V driver: %s", err)
	}

	// Set up the state.
	state := new(multistep.BasicStateBag)
	state.Put("config", &b.config)
	state.Put("driver", driver)
	state.Put("hook", hook)
	state.Put("ui", ui)

	steps := []multistep.Step{
		&hypervcommon.StepCreateTempDir{},
		&hypervcommon.StepOutputDir{
			Force: b.config.PackerForce,
			Path:  b.config.OutputDir,
		},
		&common.StepCreateFloppy{
			Files: b.config.FloppyFiles,
		},
		&hypervcommon.StepHTTPServer{
			HTTPDir:     b.config.HTTPDir,
			HTTPPortMin: b.config.HTTPPortMin,
			HTTPPortMax: b.config.HTTPPortMax,
		},
		&hypervcommon.StepCreateSwitch{
			SwitchName: b.config.SwitchName,
		},
		&hypervcommon.StepCreateVM{
			VMName:          b.config.VMName,
			SwitchName:      b.config.SwitchName,
			RamSizeMB:       b.config.RamSizeMB,
			DiskSize:        b.config.DiskSize,
			Generation:      b.config.Generation,
			Cpu:             b.config.Cpu,
			EnabeSecureBoot: b.config.EnableSecureBoot,
		},
		&hypervcommon.StepConfigureVlan{
			VlanId: b.config.VlanId,
		},
		&hypervcommon.StepEnableIntegrationService{},

		&hypervcommon.StepMountDvdDrive{
			RawSingleISOUrl: b.config.RawSingleISOUrl,
		},
		&hypervcommon.StepMountFloppydrive{},

		&hypervcommon.StepMountSecondaryDvdImages{
			Files:      b.config.SecondaryDvdImages,
			Generation: b.config.Generation,
		},

		&hypervcommon.StepRun{
			BootWait: b.config.BootWait,
			Headless: b.config.Headless,
		},

		&hypervcommon.StepTypeBootCommand{
			BootCommand: b.config.BootCommand,
			SwitchName:  b.config.SwitchName,
			Ctx:         b.config.ctx,
		},

		// configure the communicator ssh, winrm
		&communicator.StepConnect{
			Config:    &b.config.SSHConfig.Comm,
			Host:      hypervcommon.CommHost,
			SSHConfig: hypervcommon.SSHConfigFunc(&b.config.SSHConfig),
		},

		// provision requires communicator to be setup
		&common.StepProvision{},

		&hypervcommon.StepShutdown{
			Command: b.config.ShutdownCommand,
			Timeout: b.config.ShutdownTimeout,
		},

		// wait for the vm to be powered off
		&hypervcommon.StepWaitForPowerOff{},

		// remove the integration services dvd drive
		// after we power down
		&hypervcommon.StepUnmountSecondaryDvdImages{},
		&hypervcommon.StepUnmountFloppyDrive{
			Generation: b.config.Generation,
		},
		&hypervcommon.StepUnmountDvdDrive{},

		&hypervcommon.StepExportVm{
			OutputDir:      b.config.OutputDir,
			SkipCompaction: b.config.SkipCompaction,
		},

		// the clean up actions for each step will be executed reverse order
	}

	// Run the steps.
	if b.config.PackerDebug {
		b.runner = &multistep.DebugRunner{
			Steps:   steps,
			PauseFn: common.MultistepDebugFn(ui),
		}
	} else {
		b.runner = &multistep.BasicRunner{Steps: steps}
	}
	b.runner.Run(state)

	// Report any errors.
	if rawErr, ok := state.GetOk("error"); ok {
		return nil, rawErr.(error)
	}

	// If we were interrupted or cancelled, then just exit.
	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, errors.New("Build was cancelled.")
	}

	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, errors.New("Build was halted.")
	}

	return hypervcommon.NewArtifact(b.config.OutputDir)
}

// Cancel.
func (b *Builder) Cancel() {
	if b.runner != nil {
		log.Println("Cancelling the step runner...")
		b.runner.Cancel()
	}
}

func appendWarnings(slice []string, data ...string) []string {
	m := len(slice)
	n := m + len(data)
	if n > cap(slice) { // if necessary, reallocate
		// allocate double what's needed, for future growth.
		newSlice := make([]string, (n+1)*2)
		copy(newSlice, slice)
		slice = newSlice
	}
	slice = slice[0:n]
	copy(slice[m:n], data)
	return slice
}

func (b *Builder) checkDiskSize() error {
	if b.config.DiskSize == 0 {
		b.config.DiskSize = DefaultDiskSize
	}

	log.Println(fmt.Sprintf("%s: %v", "DiskSize", b.config.DiskSize))

	if b.config.DiskSize < MinDiskSize {
		return fmt.Errorf("disk_size_gb: Windows server requires disk space >= %v GB, but defined: %v", MinDiskSize, b.config.DiskSize/1024)
	} else if b.config.DiskSize > MaxDiskSize {
		return fmt.Errorf("disk_size_gb: Windows server requires disk space <= %v GB, but defined: %v", MaxDiskSize, b.config.DiskSize/1024)
	}

	return nil
}

func (b *Builder) checkRamSize() error {
	if b.config.RamSizeMB == 0 {
		b.config.RamSizeMB = DefaultRamSize
	}

	log.Println(fmt.Sprintf("%s: %v", "RamSize", b.config.RamSizeMB))

	if b.config.RamSizeMB < MinRamSize {
		return fmt.Errorf("ram_size_mb: Windows server requires memory size >= %v MB, but defined: %v", MinRamSize, b.config.RamSizeMB)
	} else if b.config.RamSizeMB > MaxRamSize {
		return fmt.Errorf("ram_size_mb: Windows server requires memory size <= %v MB, but defined: %v", MaxRamSize, b.config.RamSizeMB)
	}

	return nil
}

func (b *Builder) checkHostAvailableMemory() string {
	freeMB := powershell.GetHostAvailableMemory()

	if (freeMB - float64(b.config.RamSizeMB)) < LowRam {
		return fmt.Sprintf("Hyper-V might fail to create a VM if there is not enough free memory in the system.")
	}

	return ""
}
