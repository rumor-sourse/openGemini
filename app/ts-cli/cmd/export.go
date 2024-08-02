package cmd

import (
	"flag"
	"github.com/openGemini/openGemini/app/ts-cli/geminicli"
	"github.com/spf13/cobra"
	"os"
	"runtime/pprof"
)

const (
	CPUProfilePath = "./docs/cpu_profile"
	MEMProfilePath = "./docs/mem_profile"
)

func init() {
	rootCmd.AddCommand(exportCmd)
	exportCmd.Flags().StringVar(&options.Format, "format", "txt", "Export data format, support csv, txt.")
	exportCmd.Flags().StringVar(&options.Out, "out", "", "Destination file to export to.")
	exportCmd.Flags().StringVar(&options.DataDir, "data", "", "Data storage path to export.")
	exportCmd.Flags().StringVar(&options.WalDir, "wal", "", "WAL storage path to export.")
	exportCmd.Flags().StringVar(&options.Retentions, "retention", "", "Optional. Retention policies to export.")
	exportCmd.Flags().StringVar(&options.Host, "host", DEFAULT_HOST, "OpenGemini host to export.")
	exportCmd.Flags().IntVar(&options.Port, "port", DEFAULT_PORT, "OpenGemini port to export.")
	exportCmd.Flags().StringVar(&options.Remote, "remote", "", "Remote address to export data.")
	exportCmd.Flags().IntVar(&options.Concurrent, "concurrent", 1, "Concurrent threads number.")
	exportCmd.Flags().StringVar(&options.DBFilter, "dbfilter", "", "Optional.Databases to export.eg. db1,db2")
	exportCmd.Flags().StringVar(&options.MeasurementFilter, "mstfilter", "", "Optional.Measurements to export.eg. db1:mst1,mst2;db2:mst3")
	exportCmd.Flags().StringVar(&options.TimeFilter, "timefilter", "", "Optional.Export time range, support 'start~end'")
	exportCmd.Flags().BoolVar(&options.Compress, "compress", false, "Optional. Compress the export output.")
	err := exportCmd.MarkFlagRequired("format")
	err = exportCmd.MarkFlagRequired("out")
	if err != nil {
		return
	}
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export data from openGemini",
	Long:  `Export data from openGemini to file or remote`,
	Example: `
$ ts-cli export --format txt --out ./docs/export.txt --data /tmp/openGemini/data --wal /tmp/openGemini/data --host 127.0.0.1 
--port 8086 --dbfilter db1,db2 --mstfilter db1:mst1,mst2;db2:mst3 --timefilter "2021-01-01T00:00:00Z~2021-01-02T00:00:00Z"`,
	CompletionOptions: cobra.CompletionOptions{
		DisableDefaultCmd:   true,
		DisableDescriptions: true,
		DisableNoDescFlag:   true,
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Solved Problem: panic: BUG: memory.Allowed must be called only after flag.Parse call
		err := flag.CommandLine.Parse([]string{"-loggerLevel=ERROR"})
		if err != nil {
			return err
		}

		cpuProfile, err := os.Create(CPUProfilePath)
		if err != nil {
			return err
		}
		err = pprof.StartCPUProfile(cpuProfile)
		if err != nil {
			return err
		}
		defer pprof.StopCPUProfile()

		if err := connectCLI(); err != nil {
			return err
		}
		exportCmd := geminicli.NewExporter()
		if err := exportCmd.Export(&options); err != nil {
			return err
		}

		memProfile, err := os.Create(MEMProfilePath)
		err = pprof.Lookup("heap").WriteTo(memProfile, 0) // nolint
		if err != nil {
			return err
		}

		return nil
	},
}
