package assets

import (
	"embed"
	"io/fs"
	"text/template"
)

//go:embed all:ipxe
var ipxeFS embed.FS

//go:embed boot.ipxe.tmpl
var bootIPXETmpl string

// IPXE returns a read-only filesystem rooted at the iPXE asset dir.
// Filenames available: undionly.kpxe, snponly.efi.
func IPXE() fs.FS {
	sub, err := fs.Sub(ipxeFS, "ipxe")
	if err != nil {
		panic(err)
	}
	return sub
}

// BootIPXETemplate returns the parsed iPXE boot script template.
func BootIPXETemplate() *template.Template {
	return template.Must(template.New("boot.ipxe").Parse(bootIPXETmpl))
}
