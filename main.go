package main

import (
	_ "embed"
	"os"

	"github.com/mrhaoxx/AutoPXE/pxe"
	"github.com/mrhaoxx/AutoPXE/pxe/ipxe"
	"github.com/mrhaoxx/AutoPXE/rv"
	"github.com/mrhaoxx/AutoPXE/tftp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// readHandler is called when client starts file download from server

type Config struct {
	Version          string            `yaml:"version"`
	DefaultDistro    string            `yaml:"DefaultDistro"`
	Env              map[string]string `yaml:"Env"`
	CmdlineTemplates map[string]string `yaml:"CmdlineTemplates"`
	HostDefaults     map[string]string `yaml:"HostDefaults"`
	RootfsPath       string            `yaml:"RootfsPath"`
}

func main() {
	// use nil in place of handler to disable read or write operations

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	log.Info().Msg("Welcome to AutoPXE - A auto PXE server for clusters")

	server := tftp.NewServer()

	// Load configuration
	var cfg Config
	config, err := os.ReadFile("config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to read config file")
	}

	err = yaml.Unmarshal([]byte(config), &cfg)

	if err != nil {
		log.Fatal().Err(err).Msg("Failed to parse config file")
	}

	pxe := pxe.Server{
		RootfsPath:       cfg.RootfsPath,
		DefaultDistro:    cfg.DefaultDistro,
		Env:              cfg.Env,
		CmdlineTemplates: cfg.CmdlineTemplates,
		HostDefaults:     cfg.HostDefaults,
		DefaultDistroPattern: map[string][]pxe.ScannedBootFile{
			"debian": {
				pxe.ScannedBootFile{
					Version:    pxe.KernelVersion{Raw: "default"},
					KernelPath: "vmlinuz",
					InitrdPath: "initrd.img",
				},
			},
			"ubuntu": {
				pxe.ScannedBootFile{
					Version:    pxe.KernelVersion{Raw: "default"},
					KernelPath: "boot/vmlinuz",
					InitrdPath: "boot/initrd.img",
				},
			},
		},
	}
	ipxe := ipxe.NewServer()

	rv := rv.NewServer()

	server.Handlers = append(server.Handlers, rv)
	server.Handlers = append(server.Handlers, ipxe)
	server.Handlers = append(server.Handlers, &pxe)

	err = server.ListenAndServe(":69")
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to start TFTP server")
	}
}
