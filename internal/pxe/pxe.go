package pxe

import (
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

type Arch int

const (
	ArchUnknown Arch = iota
	ArchBIOSx86
	ArchUEFIx64
	ArchUEFIArm64
)

func (a Arch) String() string {
	switch a {
	case ArchBIOSx86:
		return "bios-x86"
	case ArchUEFIx64:
		return "uefi-x86_64"
	case ArchUEFIArm64:
		return "uefi-arm64"
	}
	return "unknown"
}

// DetectArch reads option 93 (client system architecture).
func DetectArch(req *dhcpv4.DHCPv4) Arch {
	archs := req.ClientArch()
	if len(archs) == 0 {
		return ArchUnknown
	}
	// First entry wins.
	switch archs[0] {
	case iana.INTEL_X86PC:
		return ArchBIOSx86
	case iana.EFI_IA32:
		// 32-bit UEFI is rare; treat as unknown/unsupported.
		return ArchUnknown
	case iana.EFI_BC, iana.EFI_X86_64:
		return ArchUEFIx64
	case iana.EFI_ARM64:
		return ArchUEFIArm64
	}
	return ArchUnknown
}

// IsIPXE detects whether the request originates from iPXE (post-firmware
// chainload). iPXE always sets user-class = "iPXE" and vendor-class typically
// contains the substring "iPXE".
func IsIPXE(req *dhcpv4.DHCPv4) bool {
	if uc := req.UserClass(); len(uc) > 0 {
		for _, c := range uc {
			if c == "iPXE" {
				return true
			}
		}
	}
	if vc := req.ClassIdentifier(); strings.Contains(vc, "iPXE") {
		return true
	}
	return false
}

// FirmwareBootfile returns the TFTP filename for the iPXE binary that the
// firmware should chainload, given its detected architecture.
func FirmwareBootfile(a Arch) (string, bool) {
	switch a {
	case ArchBIOSx86:
		return "undionly.kpxe", true
	case ArchUEFIx64:
		return "snponly.efi", true
	}
	return "", false
}
