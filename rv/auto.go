package rv

import (
	"bytes"
	_ "embed"
	"strings"

	"github.com/mrhaoxx/AutoPXE/tftp"
)

//go:embed bin/vmlinuz-6.6.63
var vmlinuz []byte

//go:embed bin/initrd.img-6.6.63
var initrd []byte

//go:embed bin/spacemit/6.6.63/k1-x_MUSE-Pi-Pro.dtb
var dtb []byte

//go:embed pxelinux.cfg
var pxelinux_cfg []byte

type RVServer struct {
}

func NewServer() *RVServer {
	return &RVServer{}
}

func (s *RVServer) Handle(ctx *tftp.Ctx) tftp.Ret {

	switch ctx.Path {
	case "vmlinuz-6.6.63":
		ctx.Resp.ReadFrom(bytes.NewReader(vmlinuz))
		return tftp.RequestEnd
	case "initrd.img-6.6.63":
		ctx.Resp.ReadFrom(bytes.NewReader(initrd))
		return tftp.RequestEnd
	case "spacemit/6.6.63/k1-x_MUSE-Pi-Pro.dtb":
		ctx.Resp.ReadFrom(bytes.NewReader(dtb))
		return tftp.RequestEnd
	}

	if strings.HasPrefix(ctx.Path, "pxelinux.cfg/") {
		ctx.Resp.ReadFrom(bytes.NewReader(pxelinux_cfg))
		return tftp.RequestEnd
	}

	return tftp.RequestNext
}
