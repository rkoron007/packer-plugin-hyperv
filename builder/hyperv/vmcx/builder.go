// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type Config

package vmcx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hashicorp/hcl/v2/hcldec"
	hypervcommon "github.com/hashicorp/packer-plugin-hyperv/builder/hyperv/common"
	powershell "github.com/hashicorp/packer-plugin-hyperv/builder/hyperv/common/powershell"
	"github.com/hashicorp/packer-plugin-sdk/bootcommand"
	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/shutdowncommand"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

const (
	DefaultRamSize                 = 1 * 1024    // 1GB
	MinRamSize                     = 32          // 32MB
	MaxRamSize                     = 1024 * 1024 // 1TB
	MinNestedVirtualizationRamSize = 4 * 1024    // 4GB

	LowRam = 256 // 256MB

	DefaultUsername = ""
	DefaultPassword = ""
)

// Builder implements packersdk.Builder and builds the actual Hyperv
// images.
type Builder struct {
	config Config
	runner multistep.Runner
}

type Config struct {
	common.PackerConfig            `mapstructure:",squash"`
	commonsteps.HTTPConfig         `mapstructure:",squash"`
	commonsteps.ISOConfig          `mapstructure:",squash"`
	bootcommand.BootConfig         `mapstructure:",squash"`
	hypervcommon.OutputConfig      `mapstructure:",squash"`
	hypervcommon.SSHConfig         `mapstructure:",squash"`
	hypervcommon.CommonConfig      `mapstructure:",squash"`
	shutdowncommand.ShutdownConfig `mapstructure:",squash"`
	// Packer normally halts the virtual machine after all provisioners have
	// run when no `shutdown_command` is defined. If this is set to `true`, Packer
	// *will not* halt the virtual machine but will assume that the VM will shut itself down
	// when it's done, via the preseed.cfg or your final provisioner.
	// Packer will wait for a default of 5 minutes until the virtual machine is shutdown.
	// The timeout can be changed using the `shutdown_timeout` option.
	DisableShutdown bool `mapstructure:"disable_shutdown" required:"false"`
	// The size, in megabytes, of the primary hard disk
	// for the VM. By default, this is the same as the cloned VM.
	DiskSize *uint `mapstructure:"disk_size" required:"false"`
	// This is the path to a directory containing an exported virtual machine.
	CloneFromVMCXPath string `mapstructure:"clone_from_vmcx_path"`
	// This is the name of the virtual machine to clone from.
	CloneFromVMName string `mapstructure:"clone_from_vm_name"`
	// The name of a snapshot in the
	// source machine to use as a starting point for the clone. If the value
	// given is an empty string, the last snapshot present in the source will
	// be chosen as the starting point for the new VM.
	CloneFromSnapshotName string `mapstructure:"clone_from_snapshot_name" required:"false"`
	// If set to true all snapshots
	// present in the source machine will be copied when the machine is
	// cloned. The final result of the build will be an exported virtual
	// machine that contains all the snapshots of the parent.
	CloneAllSnapshots bool `mapstructure:"clone_all_snapshots" required:"false"`
	// If true enables differencing disks. Only
	// the changes will be written to the new disk. This is especially useful if
	// your source is a VHD/VHDX. This defaults to false.
	DifferencingDisk bool `mapstructure:"differencing_disk" required:"false"`
	// When cloning a vm to build from, we run a powershell
	// Compare-VM command, which, depending on your version of Windows, may need
	// the "Copy" flag to be set to true or false. Defaults to "false". Command:
	CompareCopy bool `mapstructure:"copy_in_compare" required:"false"`

	ctx interpolate.Context
}

func (b *Builder) ConfigSpec() hcldec.ObjectSpec { return b.config.FlatMapstructure().HCL2Spec() }

func (b *Builder) Prepare(raws ...interface{}) ([]string, []string, error) {
	err := config.Decode(&b.config, &config.DecodeOpts{
		PluginType:         hypervcommon.BuilderId,
		Interpolate:        true,
		InterpolateContext: &b.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"boot_command",
			},
		},
	}, raws...)
	if err != nil {
		return nil, nil, err
	}

	// Accumulate any errors and warnings
	var errs *packersdk.MultiError
	warnings := make([]string, 0)

	if b.config.RawSingleISOUrl != "" || len(b.config.ISOUrls) > 0 {
		isoWarnings, isoErrs := b.config.ISOConfig.Prepare(&b.config.ctx)
		warnings = append(warnings, isoWarnings...)
		errs = packersdk.MultiErrorAppend(errs, isoErrs...)
	}

	errs = packersdk.MultiErrorAppend(errs, b.config.BootConfig.Prepare(&b.config.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, b.config.HTTPConfig.Prepare(&b.config.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, b.config.OutputConfig.Prepare(&b.config.ctx, &b.config.PackerConfig)...)
	errs = packersdk.MultiErrorAppend(errs, b.config.SSHConfig.Prepare(&b.config.ctx)...)
	errs = packersdk.MultiErrorAppend(errs, b.config.ShutdownConfig.Prepare(&b.config.ctx)...)

	commonErrs, commonWarns := b.config.CommonConfig.Prepare(&b.config.ctx, &b.config.PackerConfig)
	errs = packersdk.MultiErrorAppend(errs, commonErrs...)
	warnings = append(warnings, commonWarns...)

	if b.config.Cpu < 1 {
		b.config.Cpu = 1
	}

	if b.config.CloneFromVMName == "" {
		if b.config.CloneFromVMCXPath == "" {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("The clone_from_vm_name must be specified if "+
				"clone_from_vmcx_path is not specified."))
		}
	} else {
		virtualMachineExists, err := powershell.DoesVirtualMachineExist(b.config.CloneFromVMName)
		if err != nil {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Failed detecting if virtual machine to clone "+
				"from exists: %s", err))
		} else {
			if !virtualMachineExists {
				errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Virtual machine '%s' to clone from does not "+
					"exist.", b.config.CloneFromVMName))
			} else {
				b.config.Generation, err = powershell.GetVirtualMachineGeneration(b.config.CloneFromVMName)
				if err != nil {
					errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Failed detecting virtual machine to clone "+
						"from generation: %s", err))
				}

				if b.config.CloneFromSnapshotName != "" {
					virtualMachineSnapshotExists, err := powershell.DoesVirtualMachineSnapshotExist(
						b.config.CloneFromVMName, b.config.CloneFromSnapshotName)
					if err != nil {
						errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Failed detecting if virtual machine "+
							"snapshot to clone from exists: %s", err))
					} else {
						if !virtualMachineSnapshotExists {
							errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Virtual machine snapshot '%s' on "+
								"virtual machine '%s' to clone from does not exist.",
								b.config.CloneFromSnapshotName, b.config.CloneFromVMName))
						}
					}
				}

				virtualMachineOn, err := powershell.IsVirtualMachineOn(b.config.CloneFromVMName)
				if err != nil {
					errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Failed detecting if virtual machine to "+
						"clone is running: %s", err))
				} else {
					if virtualMachineOn {
						warnings = hypervcommon.Appendwarns(warnings, "Cloning from a virtual machine that is running.")
					}
				}
			}
		}
	}

	if b.config.CloneFromVMCXPath == "" {
		if b.config.CloneFromVMName == "" {
			errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("The clone_from_vmcx_path be specified if "+
				"clone_from_vm_name must is not specified."))
		}
	} else {
		if _, err := os.Stat(b.config.CloneFromVMCXPath); os.IsNotExist(err) {
			if err != nil {
				errs = packersdk.MultiErrorAppend(
					errs, fmt.Errorf("CloneFromVMCXPath does not exist: %s", err))
			}
		}
		if strings.HasSuffix(strings.ToLower(b.config.CloneFromVMCXPath), ".vmcx") {
			// User has provided the vmcx file itself rather than the containing
			// folder.
			if strings.Contains(b.config.CloneFromVMCXPath, "Virtual Machines") {
				keep := strings.Split(b.config.CloneFromVMCXPath, "Virtual Machines")
				b.config.CloneFromVMCXPath = keep[0]
			} else {
				errs = packersdk.MultiErrorAppend(errs, fmt.Errorf("Unable to "+
					"parse the clone_from_vmcx_path to find the vm directory. "+
					"Please provide the path to the folder containing the "+
					"vmcx file, not the file itself. Example: instead of "+
					"C:\\path\\to\\output-hyperv-iso\\Virtual Machines\\filename.vmcx"+
					", provide C:\\path\\to\\output-hyperv-iso\\."))
			}
		}
	}

	// Warnings

	if b.config.ShutdownCommand == "" {
		warnings = hypervcommon.Appendwarns(warnings,
			"A shutdown_command was not specified. Without a shutdown command, Packer\n"+
				"will forcibly halt the virtual machine, which may result in data loss.")
	}

	if errs != nil && len(errs.Errors) > 0 {
		return nil, warnings, errs
	}

	return nil, warnings, nil
}

// Run executes a Packer build and returns a packersdk.Artifact representing
// a Hyperv appliance.
func (b *Builder) Run(ctx context.Context, ui packersdk.Ui, hook packersdk.Hook) (packersdk.Artifact, error) {
	// Create the driver that we'll use to communicate with Hyperv
	driver, err := hypervcommon.NewHypervPS4Driver()
	if err != nil {
		return nil, fmt.Errorf("Failed creating Hyper-V driver: %s", err)
	}

	// Set up the state.
	state := new(multistep.BasicStateBag)
	state.Put("debug", b.config.PackerDebug)
	state.Put("driver", driver)
	state.Put("hook", hook)
	state.Put("ui", ui)

	steps := []multistep.Step{
		&hypervcommon.StepCreateBuildDir{
			TempPath: b.config.TempPath,
		},
		&commonsteps.StepOutputDir{
			Force: b.config.PackerForce,
			Path:  b.config.OutputDir,
		},
		&commonsteps.StepDownload{
			Checksum:    b.config.ISOChecksum,
			Description: "ISO",
			ResultKey:   "iso_path",
			Url:         b.config.ISOUrls,
			Extension:   b.config.TargetExtension,
			TargetPath:  b.config.TargetPath,
		},
		&commonsteps.StepCreateFloppy{
			Files:       b.config.FloppyFiles,
			Directories: b.config.FloppyConfig.FloppyDirectories,
			Label:       b.config.FloppyConfig.FloppyLabel,
		},
		commonsteps.HTTPServerFromHTTPConfig(&b.config.HTTPConfig),
		&hypervcommon.StepCreateSwitch{
			SwitchName: b.config.SwitchName,
		},
		&hypervcommon.StepCloneVM{
			CloneFromVMCXPath:              b.config.CloneFromVMCXPath,
			CloneFromVMName:                b.config.CloneFromVMName,
			CloneFromSnapshotName:          b.config.CloneFromSnapshotName,
			CloneAllSnapshots:              b.config.CloneAllSnapshots,
			VMName:                         b.config.VMName,
			SwitchName:                     b.config.SwitchName,
			CompareCopy:                    b.config.CompareCopy,
			RamSize:                        b.config.RamSize,
			Cpu:                            b.config.Cpu,
			EnableMacSpoofing:              b.config.EnableMacSpoofing,
			EnableDynamicMemory:            b.config.EnableDynamicMemory,
			EnableSecureBoot:               b.config.EnableSecureBoot,
			SecureBootTemplate:             b.config.SecureBootTemplate,
			EnableVirtualizationExtensions: b.config.EnableVirtualizationExtensions,
			EnableTPM:                      b.config.EnableTPM,
			MacAddress:                     b.config.MacAddress,
			KeepRegistered:                 b.config.KeepRegistered,
			AdditionalDiskSize:             b.config.AdditionalDiskSize,
			DiskBlockSize:                  b.config.DiskBlockSize,
		},

		&hypervcommon.StepResizeVhd{
			DiskSize: b.config.DiskSize,
		},

		&hypervcommon.StepEnableIntegrationService{},

		&hypervcommon.StepMountDvdDrive{
			Generation:      b.config.Generation,
			FirstBootDevice: b.config.FirstBootDevice,
		},
		&hypervcommon.StepMountFloppydrive{
			Generation: b.config.Generation,
		},

		&hypervcommon.StepMountGuestAdditions{
			GuestAdditionsMode: b.config.GuestAdditionsMode,
			GuestAdditionsPath: b.config.GuestAdditionsPath,
			Generation:         b.config.Generation,
		},
		&commonsteps.StepCreateCD{
			Files:   b.config.CDConfig.CDFiles,
			Content: b.config.CDConfig.CDContent,
			Label:   b.config.CDConfig.CDLabel,
		},
		&hypervcommon.StepMountSecondaryDvdImages{
			IsoPaths:   b.config.SecondaryDvdImages,
			Generation: b.config.Generation,
		},

		&hypervcommon.StepConfigureVlan{
			VlanId:       b.config.VlanId,
			SwitchVlanId: b.config.SwitchVlanId,
		},

		&hypervcommon.StepSetBootOrder{
			BootOrder: b.config.BootOrder,
		},
		&hypervcommon.StepSetFirstBootDevice{
			Generation:      b.config.Generation,
			FirstBootDevice: b.config.FirstBootDevice,
		},

		&hypervcommon.StepRun{
			Headless:   b.config.Headless,
			SwitchName: b.config.SwitchName,
		},

		&hypervcommon.StepTypeBootCommand{
			BootCommand:   b.config.FlatBootCommand(),
			BootWait:      b.config.BootWait,
			SwitchName:    b.config.SwitchName,
			Ctx:           b.config.ctx,
			GroupInterval: b.config.BootConfig.BootGroupInterval,
		},

		// configure the communicator ssh, winrm
		&communicator.StepConnect{
			Config:    &b.config.SSHConfig.Comm,
			Host:      hypervcommon.CommHost(b.config.SSHConfig.Comm.Host()),
			SSHConfig: b.config.SSHConfig.Comm.SSHConfigFunc(),
		},

		// provision requires communicator to be setup
		&commonsteps.StepProvision{},

		// Remove ephemeral SSH keys, if using
		&commonsteps.StepCleanupTempKeys{
			Comm: &b.config.SSHConfig.Comm,
		},

		&hypervcommon.StepShutdown{
			Command:         b.config.ShutdownCommand,
			Timeout:         b.config.ShutdownTimeout,
			DisableShutdown: b.config.DisableShutdown,
		},

		// wait for the vm to be powered off
		&hypervcommon.StepWaitForPowerOff{},

		// remove the secondary dvd images
		// after we power down
		&hypervcommon.StepUnmountSecondaryDvdImages{},
		&hypervcommon.StepUnmountGuestAdditions{},
		&hypervcommon.StepUnmountDvdDrive{},
		&hypervcommon.StepUnmountFloppyDrive{
			Generation: b.config.Generation,
		},
		&hypervcommon.StepCompactDisk{
			SkipCompaction: b.config.SkipCompaction,
		},
		&hypervcommon.StepExportVm{
			OutputDir:  b.config.OutputDir,
			SkipExport: b.config.SkipExport,
		},
		&hypervcommon.StepCollateArtifacts{
			OutputDir:  b.config.OutputDir,
			SkipExport: b.config.SkipExport,
		},
	}

	// the clean up actions for each step will be executed reverse order

	// Run the steps.
	b.runner = commonsteps.NewRunner(steps, b.config.PackerConfig, ui)
	b.runner.Run(ctx, state)

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

	generatedData := map[string]interface{}{"generated_data": state.Get("generated_data")}
	return hypervcommon.NewArtifact(b.config.OutputDir, generatedData)
}

// Cancel.
