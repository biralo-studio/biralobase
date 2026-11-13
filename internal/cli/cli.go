package cli

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/biralo-studio/biralobase/internal/bundle"
	"github.com/biralo-studio/biralobase/internal/provision"
	server "github.com/biralo-studio/biralobase/internal/runtime"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/spf13/cobra"
)

//go:embed biralobase.d.ts
var functionTypes string

func Execute(ctx context.Context, args []string, output io.Writer) error {
	c, err := provision.Load()
	if err != nil {
		return err
	}
	root := &cobra.Command{Use: "biralobase", Short: "Biralobase: a PocketBase-compatible backend on Turso", Version: server.Version, SilenceUsage: true, SilenceErrors: true}
	root.SetArgs(args)
	root.SetOut(output)
	root.SetErr(output)
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&c.AppDir, "app-dir", c.AppDir, "application source or immutable bundle directory")
	root.PersistentFlags().StringVar(&c.DataDir, "data-dir", c.DataDir, "persistent data directory")
	serve := &cobra.Command{Use: "serve", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		s, err := server.Open(cmd.Context(), c)
		if err != nil {
			return err
		}
		return s.Serve(cmd.Context())
	}}
	serve.Flags().BoolVar(&c.Dev, "dev", c.Dev, "allow mutable local application source and dashboard schema edits")
	serve.Flags().StringVar(&c.HTTP, "http", c.HTTP, "HTTP listen address (put HTTPS proxy in front in production)")
	root.AddCommand(serve)
	root.AddCommand(&cobra.Command{Use: "init [directory]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path := "."
		if len(args) == 1 {
			path = args[0]
		}
		if err := Init(path); err != nil {
			return err
		}
		fmt.Fprintln(output, "Created Biralobase application in", path)
		return nil
	}})
	schemaCmd := &cobra.Command{Use: "schema", Short: "Generate, validate, and export versioned application schema"}
	var allowDestructive bool
	generate := &cobra.Command{Use: "generate", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		path, err := schema.Generate(c.AppDir, allowDestructive)
		if err != nil {
			return err
		}
		if path == "" {
			fmt.Fprintln(output, "Schema is unchanged")
		} else {
			fmt.Fprintln(output, path)
		}
		return nil
	}}
	generate.Flags().BoolVar(&allowDestructive, "allow-destructive", false, "acknowledge generated removals, type changes, and narrowed selections/relations for review")
	schemaCmd.AddCommand(generate, &cobra.Command{Use: "check", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error { return schema.Check(c.AppDir) }})
	schemaCmd.AddCommand(&cobra.Command{Use: "export", Short: "Export local dashboard edits while the server is stopped", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error { return server.Export(c) }})
	root.AddCommand(schemaCmd)
	bundleCmd := &cobra.Command{Use: "bundle"}
	var revision, destination string
	build := &cobra.Command{Use: "build", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		_, err := bundle.Build(c.AppDir, destination, revision, server.Version, bundle.Platform())
		return err
	}}
	build.Flags().StringVar(&revision, "revision", "", "source Git commit")
	build.Flags().StringVar(&destination, "output", "", "new immutable release directory")
	_ = build.MarkFlagRequired("revision")
	_ = build.MarkFlagRequired("output")
	bundleCmd.AddCommand(build, &cobra.Command{Use: "verify", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		return bundle.Verify(c.AppDir, server.Version, bundle.Platform())
	}})
	root.AddCommand(bundleCmd)
	var healthURL string
	health := &cobra.Command{Use: "healthcheck", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
		if err != nil {
			return err
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(r)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New("server is not ready")
		}
		return nil
	}}
	health.Flags().StringVar(&healthURL, "url", "http://127.0.0.1:8090/api/biralobase/ready", "readiness endpoint")
	root.AddCommand(health)
	return root.ExecuteContext(ctx)
}

func Init(root string) error {
	if _, err := os.Stat(filepath.Join(root, schema.FileName)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("application schema already exists")
	}
	s, err := schema.Default()
	if err != nil {
		return err
	}
	if err = schema.Write(filepath.Join(root, schema.FileName), s); err != nil {
		return err
	}
	files := map[string]string{
		"package.json":              "{\n  \"private\": true,\n  \"type\": \"module\",\n  \"scripts\": {\"build\": \"tsc -p tsconfig.json\", \"test\": \"node --test\"},\n  \"devDependencies\": {\"typescript\": \"5.9.3\"}\n}\n",
		"tsconfig.json":             "{\"compilerOptions\":{\"target\":\"ES2023\",\"module\":\"NodeNext\",\"moduleResolution\":\"NodeNext\",\"strict\":true,\"outDir\":\"functions/dist\",\"rootDir\":\"functions\",\"declaration\":true},\"include\":[\"functions/*.ts\"]}\n",
		"functions/index.ts":        "import type { Application } from './biralobase.js';\nexport default {\n  functions: {\n    hello: async () => ({ status: 200, body: { message: 'Hello from Biralobase' } }),\n  },\n} satisfies Application;\n",
		"functions/biralobase.d.ts": functionTypes,
		".gitignore":                "node_modules/\nfunctions/dist/\nbb_data/\nreleases/\n.env\nsecrets/\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		_, writeErr := io.WriteString(f, body)
		if err = errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(root, "migrations"), 0755)
}
