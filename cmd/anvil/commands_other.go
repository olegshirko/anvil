package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"anvil/internal/domain"
	"anvil/internal/embedded"
	"anvil/internal/usecase"

	"github.com/docker/go-units"
	"github.com/sirupsen/logrus"
)

// listCmd

type listCmd struct {
	JSON bool `short:"j" help:"Print JSON output"`
}

func (c *listCmd) Run(g *Globals) error {
	app := g.app
	profile := app.Profile()
	profileArgs := []string{}
	if profile.Changed {
		profileArgs = append(profileArgs, profile.ID)
	}

	instances, err := app.List(context.Background(), profileArgs, c.JSON)
	if err != nil {
		return err
	}

	if c.JSON {
		enc := json.NewEncoder(os.Stdout)
		for _, inst := range instances {
			inst.Dir = "" // hide dir
			if err := enc.Encode(inst); err != nil {
				return err
			}
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 4, 8, 4, ' ', 0)
	if _, err := fmt.Fprintln(w, "PROFILE\tSTATUS\tARCH\tCPUS\tMEMORY\tDISK\tRUNTIME\tADDRESS"); err != nil {
		return err
	}
	if len(instances) == 0 {
		logrus.Warn("No instance found. Run `anvil start` to create an instance.")
	}
	for _, inst := range instances {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			inst.Name, inst.Status, inst.Arch, inst.CPU,
			units.BytesSize(float64(inst.Memory)),
			units.BytesSize(float64(inst.Disk)),
			inst.Runtime, inst.IPAddress,
		); err != nil {
			return err
		}
	}
	return w.Flush()
}

// pruneCmd

type pruneCmd struct {
	Force bool `short:"f" help:"Do not prompt for yes/no"`
	All   bool `short:"a" help:"Include Lima assets"`
}

func (c *pruneCmd) Run(g *Globals) error {
	anvilCacheDir := g.app.ConfigService().CacheDir()
	limaCacheDir := filepath.Join(filepath.Dir(anvilCacheDir), "lima")
	if !c.Force {
		msg := fmt.Sprintf("'%s' will be emptied, are you sure", anvilCacheDir)
		if c.All {
			msg = fmt.Sprintf("'%s' and '%s' will be emptied, are you sure", anvilCacheDir, limaCacheDir)
		}
		fmt.Println(msg, "[y/N]")
		var resp string
		if _, err := fmt.Scanln(&resp); err != nil {
			logrus.Warnln("error reading response:", err)
			return nil
		}
		if resp != "y" && resp != "Y" {
			return nil
		}
	}
	logrus.Info("Pruning ", anvilCacheDir)
	if err := os.RemoveAll(anvilCacheDir); err != nil {
		return fmt.Errorf("error during prune: %w", err)
	}
	return g.app.Prune(context.Background(), c.All)
}

// cloneCmd

type cloneCmd struct {
	From string `arg:"" help:"Source profile"`
	To   string `arg:"" help:"Destination profile"`
}

func (c *cloneCmd) Run(g *Globals) error {
	svc := g.app.ConfigService()
	from := svc.ProfileFromName(c.From)
	to := svc.ProfileFromName(c.To)

	logrus.Infof("preparing to clone %s...", from.DisplayName)

	// verify source profile exists
	if stat, err := os.Stat(svc.ProfileLimaInstanceDir(from)); err != nil || !stat.IsDir() {
		return fmt.Errorf("anvil profile '%s' does not exist", from.ShortName)
	}

	// verify destination profile does not exist
	if stat, err := os.Stat(svc.ProfileLimaInstanceDir(to)); err == nil && stat.IsDir() {
		return fmt.Errorf("anvil profile '%s' already exists, delete with `anvil delete %s` and try again", to.ShortName, to.ShortName)
	}

	// copy VM files
	logrus.Info("cloning virtual machine...")
	limaFrom := svc.ProfileLimaInstanceDir(from)
	limaTo := svc.ProfileLimaInstanceDir(to)
	if err := os.MkdirAll(limaTo, 0755); err != nil {
		return fmt.Errorf("error preparing to copy VM: %w", err)
	}
	files := []string{"basedisk", "diffdisk", "disk", "cidata.iso", "lima.yaml"}
	for _, f := range files {
		src := filepath.Join(limaFrom, f)
		dst := filepath.Join(limaTo, f)
		if data, err := os.ReadFile(src); err == nil {
			if err := os.WriteFile(dst, data, 0644); err != nil {
				return fmt.Errorf("error copying %s: %w", f, err)
			}
		}
	}

	// copy config
	logrus.Info("copying config...")
	configFrom := svc.ProfileConfigDir(from)
	configTo := svc.ProfileConfigDir(to)
	if err := os.MkdirAll(configTo, 0755); err != nil {
		return fmt.Errorf("cannot copy config to new profile '%s': %w", to.ShortName, err)
	}
	if data, err := os.ReadFile(filepath.Join(configFrom, "config.yaml")); err == nil {
		if err := os.WriteFile(filepath.Join(configTo, "config.yaml"), data, 0644); err != nil {
			return fmt.Errorf("error copying config: %w", err)
		}
	}

	logrus.Info("clone successful")
	logrus.Infof("run `anvil start %s` to start the newly cloned profile", to.ShortName)
	return nil
}

// templateCmd

type templateCmd struct {
	Editor string `help:"Editor to use for edit e.g. vim, nano, code"`
	Print  bool   `help:"Print out the configuration file path, without editing"`
}

func (c *templateCmd) Run(g *Globals) error {
	tmplFile := templateFile(g.app)
	if c.Print {
		fmt.Println(tmplFile)
		return nil
	}

	abort, err := embedded.ReadString("defaults/abort.yaml")
	if err != nil {
		return fmt.Errorf("error reading embedded file: %w", err)
	}
	info, err := embedded.ReadString("defaults/template.yaml")
	if err != nil {
		return fmt.Errorf("error reading embedded file: %w", err)
	}
	template, err := templateFileOrDefault(g.app)
	if err != nil {
		return fmt.Errorf("error reading template file: %w", err)
	}

	tmpFile, err := waitForUserEdit(c.Editor, []byte(abort+"\n"+info+"\n"+template))
	if err != nil {
		return fmt.Errorf("error editing template file: %w", err)
	}
	if tmpFile == "" {
		return fmt.Errorf("empty file, template edit aborted")
	}
	defer func() { _ = os.Remove(tmpFile) }()

	cf, err := g.app.ConfigService().LoadFromFile(tmpFile)
	if err != nil {
		return fmt.Errorf("error in template: %w", err)
	}
	if err := g.app.ConfigService().SaveTemplate(cf, tmplFile); err != nil {
		return fmt.Errorf("error saving template: %w", err)
	}

	logrus.Println("configurations template saved")
	return nil
}

func templateFile(app usecase.Application) string {
	return filepath.Join(app.ConfigService().TemplatesDir(), "default.yaml")
}

func templateFileOrDefault(app usecase.Application) (string, error) {
	tFile := templateFile(app)
	if _, err := os.Stat(tFile); err == nil {
		b, err := os.ReadFile(tFile)
		if err == nil {
			return string(b), nil
		}
	}
	return embedded.ReadString("defaults/anvil.yaml")
}

// nerdctlCmd

type nerdctlCmd struct {
	Args []string `arg:"" optional:"" help:"nerdctl args"`
}

func (c *nerdctlCmd) Run(g *Globals) error {
	r, err := g.app.Runtime(context.Background())
	if err != nil {
		return err
	}
	if r != domain.RuntimeContainerd {
		return fmt.Errorf("nerdctl only supports %s runtime", domain.RuntimeContainerd)
	}
	nerdctlArgs := append([]string{"sudo", "nerdctl"}, c.Args...)
	return g.app.SSH(context.Background(), nerdctlArgs...)
}
