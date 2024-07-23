package geminicli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/golang/snappy"
	"github.com/openGemini/openGemini/engine"
	"github.com/openGemini/openGemini/engine/immutable"
	"github.com/openGemini/openGemini/engine/index/tsi"
	"github.com/openGemini/openGemini/lib/bufferpool"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/fileops"
	"github.com/openGemini/openGemini/lib/index"
	"github.com/openGemini/openGemini/lib/record"
	"github.com/openGemini/openGemini/lib/util"
	"github.com/openGemini/openGemini/lib/util/lifted/vm/protoparser/influx"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	tsspFileExtension = "tssp"
	walFileExtension  = "wal"
	csvFormatExporter = "csv"
	txtFormatExporter = "txt"
	dirNameSeparator  = "_"
	stdOtuMark        = "-"
	writerBufferSize  = 1024 * 1024
)

type DataFilter struct {
	measurements map[string]struct{}
	startTime    int64
	endTime      int64
}

func NewDataFilter(mstFilter string) *DataFilter {
	msts := strings.Split(mstFilter, ",")
	mstNames := make(map[string]struct{})
	for _, mst := range msts {
		if len(mst) == 0 {
			continue
		}
		mstNames[mst] = struct{}{}
	}
	return &DataFilter{
		measurements: mstNames,
		startTime:    0,
		endTime:      0,
	}
}

func (d *DataFilter) parseTime(clc *CommandLineConfig) error {
	var start, end string
	timeSlot := strings.Split(clc.TimeFilter, "~")
	if len(timeSlot) == 2 {
		start = timeSlot[0]
		end = timeSlot[1]
	}
	// set defaults
	if start != "" {
		s, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return err
		}
		d.startTime = s.UnixNano()
	} else {
		d.startTime = math.MinInt64
	}

	if end != "" {
		e, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return err
		}
		d.endTime = e.UnixNano()
	} else {
		// set end time to max if it is not set.
		d.endTime = math.MaxInt64
	}

	if d.startTime > d.endTime {
		return fmt.Errorf("start time `%q` > end time `%q`", start, end)
	}

	return nil
}

func (d *DataFilter) filter(t int64) bool {
	return t >= d.startTime && t <= d.endTime
}

func (d *DataFilter) isBelowMinFilter(t int64) bool {
	return t < d.startTime
}

func (d *DataFilter) isAboveMaxFilter(t int64) bool {
	return t > d.endTime
}

type DatabaseDiskInfo struct {
	dbName          string              // ie. "NOAA_water_database"
	rps             map[string]struct{} // ie. ["0:autogen","1:every_one_day"]
	dataDir         string              // ie. "/tmp/openGemini/data/data/NOAA_water_database"
	walDir          string              // ie. "/tmp/openGemini/data/wal/NOAA_water_database"
	rpToTsspDirMap  map[string]string   // ie. {"0:autogen", "/tmp/openGemini/data/data/NOAA_water_database/0/autogen"}
	rpToWalDirMap   map[string]string   // ie. {"0:autogen", "/tmp/openGemini/data/wal/NOAA_water_database/0/autogen"}
	rpToIndexDirMap map[string]string   // ie. {"0:autogen", "/tmp/openGemini/data/data/NOAA_water_database/0/autogen/index"}
}

func newDatabaseDiskInfo() *DatabaseDiskInfo {
	return &DatabaseDiskInfo{
		rps:             make(map[string]struct{}),
		rpToTsspDirMap:  make(map[string]string),
		rpToWalDirMap:   make(map[string]string),
		rpToIndexDirMap: make(map[string]string),
	}
}

func (d *DatabaseDiskInfo) init(actualDataDir string, actualWalDir string, databaseName string, retentionPolicy string) error {
	d.dbName = databaseName

	// check whether the database is in actualDataPath
	dataDir := path.Join(actualDataDir, databaseName)
	if _, err := os.Stat(dataDir); err != nil {
		return err
	}
	// check whether the database is in actualWalPath
	walDir := path.Join(actualWalDir, databaseName)
	if _, err := os.Stat(walDir); err != nil {
		return err
	}

	// ie. /tmp/openGemini/data/data/my_db  /tmp/openGemini/data/wal/my_db
	d.dataDir, d.walDir = dataDir, walDir

	ptDirs, err := os.ReadDir(d.dataDir)
	if err != nil {
		return err
	}
	for _, ptDir := range ptDirs {
		// ie. /tmp/openGemini/data/data/my_db/0
		ptTsspPath := path.Join(d.dataDir, ptDir.Name())
		// ie. /tmp/openGemini/data/wal/my_db/0
		ptWalPath := path.Join(d.walDir, ptDir.Name())

		if retentionPolicy != "" {
			rpNames := strings.Split(retentionPolicy, ",")
			for _, rpName := range rpNames {
				ptWithRp := ptDir.Name() + ":" + rpName
				rpTsspPath := path.Join(ptTsspPath, rpName)
				if _, err := os.Stat(rpTsspPath); err != nil {
					return fmt.Errorf("retention policy %q invalid : %s", retentionPolicy, err)
				} else {
					d.rps[ptWithRp] = struct{}{}
					d.rpToTsspDirMap[ptWithRp] = rpTsspPath
					d.rpToIndexDirMap[ptWithRp] = path.Join(rpTsspPath, "index")
				}
				rpWalPath := path.Join(ptWalPath, rpName)
				if _, err := os.Stat(rpWalPath); err != nil {
					return fmt.Errorf("retention policy %q invalid : %s", retentionPolicy, err)
				} else {
					d.rpToWalDirMap[ptWithRp] = rpWalPath
				}
			}
			continue
		}

		rpTsspDirs, err1 := os.ReadDir(ptTsspPath)
		if err1 != nil {
			return err
		}
		for _, rpDir := range rpTsspDirs {
			if rpDir.IsDir() {
				ptWithRp := ptDir.Name() + ":" + rpDir.Name()
				rpPath := path.Join(ptTsspPath, rpDir.Name())
				d.rps[ptWithRp] = struct{}{}
				d.rpToTsspDirMap[ptWithRp] = rpPath
				d.rpToIndexDirMap[ptWithRp] = path.Join(rpPath, "index")
			}
		}

		rpWalDirs, err2 := os.ReadDir(ptWalPath)
		if err2 != nil {
			return err
		}
		for _, rpDir := range rpWalDirs {
			ptWithRp := ptDir.Name() + ":" + rpDir.Name()
			if rpDir.IsDir() {
				rpPath := path.Join(ptWalPath, rpDir.Name())
				d.rpToWalDirMap[ptWithRp] = rpPath
			}
		}
	}
	return nil
}

type Exporter struct {
	exportFormat      string
	databases         string
	databaseDiskInfos []*DatabaseDiskInfo
	actualDataPath    string
	actualWalPath     string
	outPutPath        string
	retentions        string
	filter            *DataFilter
	compress          bool
	lineCount         uint64
	Parser

	stderrLogger  *log.Logger
	stdoutLogger  *log.Logger
	defaultLogger *log.Logger

	manifest                        map[string]struct{}                      // {dbName:rpName, struct{}{}}
	rpNameToMeasurementTsspFilesMap map[string]map[string][]string           // {dbName:rpName, {measurementName, tssp file absolute path}}
	rpNameToIdToIndexMap            map[string]map[uint64]*tsi.MergeSetIndex // {dbName:rpName, {indexId, *mergeSetIndex}}
	rpNameToWalFilesMap             map[string][]string                      // {dbName:rpName:shardDurationRange, index file absolute path}

	Stderr io.Writer
	Stdout io.Writer
}

func NewExporter() *Exporter {
	return &Exporter{
		stderrLogger: log.New(os.Stderr, "export: ", log.LstdFlags),
		stdoutLogger: log.New(os.Stdout, "export: ", log.LstdFlags),

		manifest:                        make(map[string]struct{}),
		rpNameToMeasurementTsspFilesMap: make(map[string]map[string][]string),
		rpNameToIdToIndexMap:            make(map[string]map[uint64]*tsi.MergeSetIndex),
		rpNameToWalFilesMap:             make(map[string][]string),

		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// usingStdOut return if this export task uses stdout to receive results.
func (e *Exporter) usingStdOut() bool {
	return e.outPutPath == stdOtuMark
}

// parseActualDir transforms user puts in datadir and waldir to actual dirs
func (e *Exporter) parseActualDir(clc *CommandLineConfig) error {
	actualDataDir := path.Join(clc.DataDir, config.DataDirectory)
	if _, err := os.Stat(actualDataDir); err != nil {
		return err
	} else {
		e.actualDataPath = actualDataDir
	}

	actualWalDir := path.Join(clc.WalDir, config.WalDirectory)
	if _, err := os.Stat(actualWalDir); err != nil {
		return err
	} else {
		e.actualWalPath = actualWalDir
	}

	return nil
}

// parseDatabaseInfos get all path infos for export.
func (e *Exporter) parseDatabaseInfos() error {
	// If the user does not specify a database, find all database and RP information
	if e.databases == "" {
		if e.retentions != "" {
			return fmt.Errorf("retention policies can only be specified when specifying a database separately")
		}
		// If user doesn't specified a database, get all db's path info.
		files, err := os.ReadDir(e.actualDataPath)
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() {
				dbDiskInfo := newDatabaseDiskInfo()
				err := dbDiskInfo.init(e.actualDataPath, e.actualWalPath, file.Name(), "")
				if err != nil {
					return err
				}
				e.databaseDiskInfos = append(e.databaseDiskInfos, dbDiskInfo)
			}
		}
		return nil
	}

	dbNames := strings.Split(e.databases, ",")
	// If the user specifies multiple databases, find info one by one
	if len(dbNames) > 1 {
		if e.retentions != "" {
			return fmt.Errorf("retention policies can only be specified when specifying only one database separately")
		}
		for _, dbName := range dbNames {
			dbDiskInfo := newDatabaseDiskInfo()
			err := dbDiskInfo.init(e.actualDataPath, e.actualWalPath, dbName, "")
			if err != nil {
				return fmt.Errorf("can't find database files for %s : %s", dbName, err)
			}
			e.databaseDiskInfos = append(e.databaseDiskInfos, dbDiskInfo)
		}
		return nil
	}

	// If the user specifies only one database, but specifies multiple retentions, find info one by one
	if e.retentions != "" {
		dbDiskInfo := newDatabaseDiskInfo()
		err := dbDiskInfo.init(e.actualDataPath, e.actualWalPath, dbNames[0], e.retentions)
		if err != nil {
			return fmt.Errorf("can't find database files for %s : %s", dbNames[0], err)
		}
		e.databaseDiskInfos = append(e.databaseDiskInfos, dbDiskInfo)
		return nil
	}

	// If the user specifies only one database, and doesn't specify retentions.
	dbDiskInfo := newDatabaseDiskInfo()
	err := dbDiskInfo.init(e.actualDataPath, e.actualWalPath, dbNames[0], "")
	if err != nil {
		return fmt.Errorf("can't find database files for %s : %s", dbNames[0], err)
	}
	e.databaseDiskInfos = append(e.databaseDiskInfos, dbDiskInfo)
	return nil
}

// Init inits the Exporter instance ues CommandLineConfig specific by user
func (e *Exporter) Init(clc *CommandLineConfig) error {
	e.exportFormat = clc.Format
	if e.exportFormat == csvFormatExporter {
		e.Parser = &CsvParser{}
	} else if e.exportFormat == txtFormatExporter {
		e.Parser = &TxtParser{}
	} else {
		return fmt.Errorf("unsupported export format %q", e.exportFormat)
	}
	e.databases = clc.DBFilter
	e.retentions = clc.Retentions
	e.outPutPath = clc.Out
	e.compress = clc.Compress
	// filter dbs, msts, time
	e.filter = NewDataFilter(clc.MeasurementFilter)

	// If output fd is stdout.
	if e.usingStdOut() {
		e.defaultLogger = e.stderrLogger
	} else {
		e.defaultLogger = e.stdoutLogger
	}

	if err := e.filter.parseTime(clc); err != nil {
		return err
	}

	// ie. dataDir=/tmp/openGemini/data               walDir=/tmp/openGemini/data
	//     actualDataPath=/tmp/openGemini/data/data    actualWalPath=/tmp/openGemini/data/wal
	if err := e.parseActualDir(clc); err != nil {
		return err
	}

	// Get all dir infos that we need,like all database/rp/tsspDirs and database/rp/walDirs
	if err := e.parseDatabaseInfos(); err != nil {
		return err
	}

	return nil
}

// Export exports all data user want.
func (e *Exporter) Export(clc *CommandLineConfig) error {
	if err := e.Init(clc); err != nil {
		return err
	}

	for _, dbDiskInfo := range e.databaseDiskInfos {
		err := e.walkDatabase(dbDiskInfo)
		if err != nil {
			return err
		}
	}
	return e.write()
}

// walkDatabase gets all db's tssp filepath, wal filepath, and index filepath.
func (e *Exporter) walkDatabase(dbDiskInfo *DatabaseDiskInfo) error {
	if err := e.walkTsspFile(dbDiskInfo); err != nil {
		return err
	}
	if err := e.walkIndexFiles(dbDiskInfo); err != nil {
		return err
	}
	if err := e.walkWalFile(dbDiskInfo); err != nil {
		return err
	}

	for _, idxMap := range e.rpNameToIdToIndexMap {
		for _, idx := range idxMap {
			err := idx.Open()
			if err != nil {
				panic(err)
			}
		}
	}
	return nil
}

// write writes data to output fd user specifics.
func (e *Exporter) write() error {
	var outputWriter io.Writer
	if e.usingStdOut() {
		outputWriter = e.Stdout
	} else {
		outputFile, err := os.Create(e.outPutPath)
		if err != nil {
			return err
		}
		defer func(outputFile *os.File) {
			_ = outputFile.Close()
		}(outputFile)

		outputWriter = outputFile
	}

	// 1mb buffer size to sync file
	bufWriter := bufio.NewWriterSize(outputWriter, writerBufferSize)
	defer func(bufWriter *bufio.Writer) {
		_ = bufWriter.Flush()
	}(bufWriter)

	outputWriter = bufWriter

	if e.compress {
		gzipWriter := gzip.NewWriter(outputWriter)
		defer func(gzipWriter *gzip.Writer) {
			_ = gzipWriter.Close()
		}(gzipWriter)
		outputWriter = gzipWriter
	}

	// metaWriter to write information that are not line-protocols
	metaWriter := outputWriter

	return e.writeFull(metaWriter, outputWriter)
}

// writeFull writes all DDL and DML
func (e *Exporter) writeFull(metaWriter io.Writer, outputWriter io.Writer) error {
	start, end := time.Unix(0, e.filter.startTime).UTC().Format(time.RFC3339), time.Unix(0, e.filter.endTime).UTC().Format(time.RFC3339)
	fmt.Fprintf(metaWriter, "# openGemini EXPORT: %s - %s\n\n", start, end)

	if err := e.writeDDL(metaWriter, outputWriter); err != nil {
		return err
	}

	if err := e.writeDML(metaWriter, outputWriter); err != nil {
		return err
	}

	e.defaultLogger.Printf("Summarize %d line protocol\n", e.lineCount)
	return nil
}

// walkTsspFile walk all tssp files for every database.
func (e *Exporter) walkTsspFile(dbDiskInfo *DatabaseDiskInfo) error {
	for ptWithRp := range dbDiskInfo.rps {
		rpDir := dbDiskInfo.rpToTsspDirMap[ptWithRp]
		if err := filepath.Walk(rpDir, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if filepath.Ext(path) != "."+tsspFileExtension {
				return nil
			}
			//search .tssp file
			tsspPathSplits := strings.Split(path, string(byte(os.PathSeparator)))
			measurementDirWithVersion := tsspPathSplits[len(tsspPathSplits)-2]
			measurementName := influx.GetOriginMstName(measurementDirWithVersion)
			_, ok := e.filter.measurements[measurementName]
			if len(e.filter.measurements) != 0 && !ok {
				return nil
			}
			// eg. "0:autogen" to ["0","autogen"]
			splitPtWithRp := strings.Split(ptWithRp, ":")
			key := dbDiskInfo.dbName + ":" + splitPtWithRp[1]
			e.manifest[key] = struct{}{}
			if _, ok := e.rpNameToMeasurementTsspFilesMap[key]; !ok {
				e.rpNameToMeasurementTsspFilesMap[key] = make(map[string][]string)
			}
			e.rpNameToMeasurementTsspFilesMap[key][measurementName] = append(e.rpNameToMeasurementTsspFilesMap[key][measurementName], path)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Exporter) walkWalFile(dbDiskInfo *DatabaseDiskInfo) error {
	for ptWithRp := range dbDiskInfo.rps {
		rpDir := dbDiskInfo.rpToWalDirMap[ptWithRp]
		if err := filepath.Walk(rpDir, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if filepath.Ext(path) != "."+walFileExtension {
				return nil
			}
			//eg. "0:autogen" to ["0","autogen"]
			splitPtWithRp := strings.Split(ptWithRp, ":")
			key := dbDiskInfo.dbName + ":" + splitPtWithRp[1]
			e.manifest[key] = struct{}{}
			e.rpNameToWalFilesMap[key] = append(e.rpNameToWalFilesMap[key], path)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Exporter) walkIndexFiles(dbDiskInfo *DatabaseDiskInfo) error {
	for ptWithRp := range dbDiskInfo.rps {
		indexPath := dbDiskInfo.rpToIndexDirMap[ptWithRp]
		files, err := os.ReadDir(indexPath)
		if err != nil {
			return err
		}
		for _, file := range files {
			if file.IsDir() {
				indexId, err2 := parseIndexDir(file.Name())
				if err2 != nil {
					return err2
				}
				// eg. "0:autogen" to ["0","autogen"]
				splitPtWithRp := strings.Split(ptWithRp, ":")
				key := dbDiskInfo.dbName + ":" + splitPtWithRp[1]
				lockPath := ""
				opt := &tsi.Options{}
				opt.Path(path.Join(indexPath, file.Name()))
				opt.IndexType(index.MergeSet)
				opt.Lock(&lockPath)
				if _, ok := e.rpNameToIdToIndexMap[key]; !ok {
					e.rpNameToIdToIndexMap[key] = make(map[uint64]*tsi.MergeSetIndex)
				}
				e.manifest[key] = struct{}{}
				if e.rpNameToIdToIndexMap[key][indexId], err = tsi.NewMergeSetIndex(opt); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeDDL write every "database:retention policy" DDL
func (e *Exporter) writeDDL(metaWriter io.Writer, outputWriter io.Writer) error {
	fmt.Fprintf(metaWriter, "# DDL\n\n")
	for _, dbDiskInfo := range e.databaseDiskInfos {
		avoidRepetition := map[string]struct{}{}
		databaseName := dbDiskInfo.dbName
		fmt.Fprintf(outputWriter, "CREATE DATABASE %s\n", databaseName)
		for ptWithRp := range dbDiskInfo.rps {
			rpName := strings.Split(ptWithRp, ":")[1]
			if _, ok := avoidRepetition[rpName]; !ok {
				fmt.Fprintf(outputWriter, "CREATE RETENTION POLICY %s ON %s DURATION 0s REPLICATION 1\n", rpName, databaseName)
				avoidRepetition[rpName] = struct{}{}
			}
		}
		fmt.Fprintf(outputWriter, "\n")
	}
	return nil
}

// writeDML write every "database:retention policy" DDL
func (e *Exporter) writeDML(metaWriter io.Writer, outputWriter io.Writer) error {
	fmt.Fprintf(metaWriter, "# DML\n\n")
	var curDatabaseName string
	// write DML for every item which key = "database:retention policy"
	for key := range e.manifest {
		keySplits := strings.Split(key, ":")

		if keySplits[0] != curDatabaseName {
			fmt.Fprintf(metaWriter, "# CONTEXT-DATABASE: %s\n\n", keySplits[0])
			curDatabaseName = keySplits[0]
		}

		// shardKeyToIndexMap stores all indexes for this "database:retention policy"
		shardKeyToIndexMap, ok := e.rpNameToIdToIndexMap[key]
		if !ok {
			return fmt.Errorf("cant find rpNameToIdToIndexMap for %q", key)
		}

		fmt.Fprintf(metaWriter, "# CONTEXT-RETENTION-POLICY: %s\n\n", keySplits[1])

		// Write all tssp files from this "database:retention policy"
		if measurementToTsspFileMap, ok := e.rpNameToMeasurementTsspFilesMap[key]; ok {
			e.defaultLogger.Printf("writing out tssp file data for %s...\n", key)
			if err := e.writeAllTsspFilesInRp(metaWriter, outputWriter, measurementToTsspFileMap, shardKeyToIndexMap); err != nil {
				return err
			}
			e.defaultLogger.Println("complete.")
		}

		// Write all wal files from this "database:retention policy"
		if files, ok := e.rpNameToWalFilesMap[key]; ok {
			e.defaultLogger.Printf("writing out wal file data for %s...\n", key)
			if err := e.writeAllWalFilesInRp(metaWriter, outputWriter, files); err != nil {
				return err
			}
			e.defaultLogger.Println("complete.")
		}
	}
	return nil
}

// writeAllTsspFilesInRp writes all tssp files in a "database:retention policy"
func (e *Exporter) writeAllTsspFilesInRp(metaWriter io.Writer, outputWriter io.Writer, measurementFilesMap map[string][]string, indexesMap map[uint64]*tsi.MergeSetIndex) error {
	fmt.Fprintf(metaWriter, "# FROM TSSP FILE.\n\n")
	var isOrder bool
	hasWrittenMstInfo := make(map[string]bool)
	for measurementName, files := range measurementFilesMap {
		fmt.Fprintf(metaWriter, "# CONTEXT-MEASUREMENT: %s \n", measurementName)
		hasWrittenMstInfo[measurementName] = false
		for _, file := range files {
			splits := strings.Split(file, string(os.PathSeparator))
			var shardDir string
			if strings.Contains(file, "out-of-order") {
				isOrder = false
				// ie./tmp/openGemini/data/data/db1/0/autogen/1_1567382400000000000_1567987200000000000_1/tssp/average_temperature_0000/out-of-order/00000002-0000-00000000.tssp
				shardDir = splits[len(splits)-5]
			} else {
				isOrder = true
				// ie./tmp/openGemini/data/data/db1/0/autogen/1_1567382400000000000_1567987200000000000_1/tssp/average_temperature_0000/00000002-0000-00000000.tssp
				shardDir = splits[len(splits)-4]
			}
			_, indexId, err := parseShardDir(shardDir)
			if err != nil {
				return err
			}
			if !hasWrittenMstInfo[measurementName] {
				if err := e.Parser.WriteMstInfo(metaWriter, outputWriter, file, isOrder, indexesMap[indexId]); err != nil {
					return err
				}
				hasWrittenMstInfo[measurementName] = true
			}
			if err := e.writeSingleTsspFile(file, outputWriter, indexesMap[indexId], isOrder); err != nil {
				return err
			}
		}
		fmt.Fprintf(outputWriter, "\n")
	}
	return nil
}

// writeSingleTsspFile writes a single tssp file's all records.
func (e *Exporter) writeSingleTsspFile(filePath string, outputWriter io.Writer, index *tsi.MergeSetIndex, isOrder bool) error {
	lockPath := ""
	tsspFile, err := immutable.OpenTSSPFile(filePath, &lockPath, isOrder, false)
	defer util.MustClose(tsspFile)

	if err != nil {
		return err
	}
	fi := immutable.NewFileIterator(tsspFile, immutable.CLog)
	itr := immutable.NewChunkIterator(fi)

	var maxTime int64
	var minTime int64
	for {
		if !itr.Next() {
			break
		}
		sid := itr.GetSeriesID()
		if sid == 0 {
			return fmt.Errorf("series ID is zero")
		}
		rec := itr.GetRecord()
		record.CheckRecord(rec)

		maxTime = rec.MaxTime(true)
		minTime = rec.MinTime(true)

		// Check if the maximum and minimum time of records that the SID points to are in the filter range of e.filter
		if e.filter.isBelowMinFilter(maxTime) || e.filter.isAboveMaxFilter(minTime) {
			continue
		}

		if err := e.writeSeriesRecords(outputWriter, sid, rec, index); err != nil {
			return err
		}
	}

	return nil
}

// writeSeriesRecords writes all records pointed to by one sid.
func (e *Exporter) writeSeriesRecords(outputWriter io.Writer, sid uint64, rec *record.Record, index *tsi.MergeSetIndex) error {

	var combineKey []byte
	var seriesKeys [][]byte
	var isExpectSeries []bool
	var err error
	// Use sid get series key's []byte
	if seriesKeys, _, isExpectSeries, err = index.SearchSeriesWithTagArray(sid, seriesKeys, nil, combineKey, isExpectSeries, nil); err != nil {
		return err
	}
	series := make([][]byte, 1)
	sIndex := 0
	for i := range seriesKeys {
		if !isExpectSeries[i] {
			continue
		}
		if sIndex >= 1 {
			bufSeries := influx.GetBytesBuffer()
			bufSeries, err = e.Parser.Parse2SeriesKeyWithoutVersion(seriesKeys[i], bufSeries, false)
			if err != nil {
				return err
			}
			series = append(series, bufSeries)
		} else {
			if series[sIndex] == nil {
				series[sIndex] = influx.GetBytesBuffer()
			}
			series[sIndex], err = e.Parser.Parse2SeriesKeyWithoutVersion(seriesKeys[i], series[sIndex][:0], false)
			if err != nil {
				return err
			}
			sIndex++
		}

	}

	var recs []record.Record
	recs = rec.Split(recs, 1)
	buf := influx.GetBytesBuffer()
	for _, r := range recs {
		if buf, err = e.writeSingleRecord(outputWriter, series, r, buf); err != nil {
			return err
		}
	}
	return nil
}

// writeSingleRecord parses a record and a series key to line protocol, and writes it.
func (e *Exporter) writeSingleRecord(outputWriter io.Writer, seriesKey [][]byte, rec record.Record, buf []byte) ([]byte, error) {
	tm := rec.Times()[0]
	if !e.filter.filter(tm) {
		return buf, nil
	}
	buf = bytes.Join(seriesKey, []byte(","))
	buf = append(buf, ' ')
	buf, err := e.Parser.AppendFields(rec, buf)
	if err != nil {
		return nil, err
	}
	if _, err := outputWriter.Write(buf); err != nil {
		return buf, err
	}
	e.lineCount++
	buf = buf[:0]
	return buf, nil
}

// writeAllWalFilesInRp writes all wal files in a "database:retention policy"
func (e *Exporter) writeAllWalFilesInRp(metaWriter io.Writer, outputWriter io.Writer, files []string) error {
	fmt.Fprintf(metaWriter, "# FROM WAL FILE.\n\n")
	for _, file := range files {
		if err := e.writeSingleWalFile(file, outputWriter); err != nil {
			return err
		}
	}
	fmt.Fprintf(outputWriter, "\n")
	return nil
}

// writeSingleWalFile writes a single wal file's all rows.
func (e *Exporter) writeSingleWalFile(file string, outputWriter io.Writer) error {
	lockPath := fileops.FileLockOption("")
	priority := fileops.FilePriorityOption(fileops.IO_PRIORITY_NORMAL)
	fd, err := fileops.OpenFile(file, os.O_RDONLY, 0640, lockPath, priority)
	defer util.MustClose(fd)
	if err != nil {
		return err
	}

	stat, err := fd.Stat()
	if err != nil {
		return err
	}
	fileSize := stat.Size()
	if fileSize == 0 {
		return nil
	}
	recordCompBuff := bufferpool.NewByteBufferPool(engine.WalCompBufSize, 0, bufferpool.MaxLocalCacheLen).Get()
	var offset int64 = 0
	var rows []influx.Row
	for {
		rows, offset, recordCompBuff, err = e.readWalRows(fd, offset, fileSize, recordCompBuff)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return nil
		}
		if err = e.writeRows(rows, outputWriter); err != nil {
			return err
		}
	}
}

// readWalRows read some rows from the fd, and reuse recordCompBuff to save memory.
func (e *Exporter) readWalRows(fd fileops.File, offset, fileSize int64, recordCompBuff []byte) ([]influx.Row, int64, []byte, error) {
	if offset >= fileSize {
		return nil, offset, recordCompBuff, io.EOF
	}

	// read record header
	var recordHeader [engine.WalRecordHeadSize]byte
	n, err := fd.ReadAt(recordHeader[:], offset)
	if err != nil {
		e.stderrLogger.Println(errno.NewError(errno.ReadWalFileFailed, fd.Name(), offset, "record header").Error())
		return nil, offset, recordCompBuff, io.EOF
	}
	if n != engine.WalRecordHeadSize {
		e.stderrLogger.Println(errno.NewError(errno.WalRecordHeaderCorrupted, fd.Name(), offset).Error())
		return nil, offset, recordCompBuff, io.EOF
	}
	offset += int64(len(recordHeader))

	// prepare record memory
	compBinaryLen := binary.BigEndian.Uint32(recordHeader[1:engine.WalRecordHeadSize])
	recordCompBuff = bufferpool.Resize(recordCompBuff, int(compBinaryLen))

	// read record body
	var recordBuff []byte
	n, err = fd.ReadAt(recordCompBuff, offset)
	if err == nil || err == io.EOF {
		offset += int64(n)
		var innerErr error
		recordBuff, innerErr = snappy.Decode(recordBuff, recordCompBuff)
		if innerErr != nil {
			e.stderrLogger.Println(errno.NewError(errno.DecompressWalRecordFailed, fd.Name(), offset, innerErr.Error()).Error())
			return nil, offset, recordCompBuff, io.EOF
		}
		var rows []influx.Row
		var tagPools []influx.Tag
		var fieldPools []influx.Field
		var indexKeyPools []byte
		var indexOptionPools []influx.IndexOption
		var err error

		rows, _, _, _, _, innerErr = influx.FastUnmarshalMultiRows(recordBuff, rows, tagPools, fieldPools, indexOptionPools, indexKeyPools)

		if innerErr == nil {
			return rows, offset, recordCompBuff, err
		}
		return rows, offset, recordCompBuff, innerErr
	}
	e.stderrLogger.Println(errno.NewError(errno.ReadWalFileFailed, fd.Name(), offset, "record body").Error())
	return nil, offset, recordCompBuff, io.EOF
}

// writeRows process a cluster of rows
func (e *Exporter) writeRows(rows []influx.Row, outputWriter io.Writer) error {
	buf := influx.GetBytesBuffer()
	var err error
	for _, r := range rows {
		if buf, err = e.writeSingleRow(r, outputWriter, buf); err != nil {
			return err
		}
	}
	return nil
}

// writeSingleRow parse a single row to lint protocol, and writes it.
func (e *Exporter) writeSingleRow(row influx.Row, outputWriter io.Writer, buf []byte) ([]byte, error) {
	measurementWithVersion := row.Name
	measurementName := influx.GetOriginMstName(measurementWithVersion)
	measurementName = EscapeMstName(measurementName)
	tags := row.Tags
	fields := row.Fields
	tm := row.Timestamp

	if !e.filter.filter(tm) {
		return buf, nil
	}

	buf = []byte(measurementName)
	buf = append(buf, ',')
	for i, tag := range tags {
		buf = append(buf, EscapeTagKey(tag.Key)+"="...)
		buf = append(buf, EscapeTagValue(tag.Value)...)
		if i != len(tags)-1 {
			buf = append(buf, ',')
		} else {
			buf = append(buf, ' ')
		}
	}
	for i, field := range fields {
		buf = append(buf, EscapeFieldKey(field.Key)+"="...)
		switch field.Type {
		case influx.Field_Type_Float:
			buf = strconv.AppendFloat(buf, field.NumValue, 'g', -1, 64)
		case influx.Field_Type_Int:
			buf = strconv.AppendInt(buf, int64(field.NumValue), 10)
			buf = append(buf, 'i')
		case influx.Field_Type_Boolean:
			buf = strconv.AppendBool(buf, field.NumValue == 1)
		case influx.Field_Type_String:
			buf = append(buf, '"')
			buf = append(buf, EscapeStringFieldValue(field.StrValue)...)
			buf = append(buf, '"')
		default:
			// This shouldn't be possible, but we'll format it anyway.
			buf = append(buf, fmt.Sprintf("%v", field)...)
		}
		if i != len(fields)-1 {
			buf = append(buf, ',')
		} else {
			buf = append(buf, ' ')
		}
	}
	buf = strconv.AppendInt(buf, tm, 10)
	buf = append(buf, '\n')
	if _, err := outputWriter.Write(buf); err != nil {
		return buf, err
	}
	e.lineCount++
	buf = buf[:0]
	return buf, nil
}

type Parser interface {
	Parse2SeriesKeyWithoutVersion(key []byte, dst []byte, splitWithNull bool) ([]byte, error)
	AppendFields(rec record.Record, buf []byte) ([]byte, error)
	WriteMstInfo(metaWriter io.Writer, outputWriter io.Writer, filePath string, isOrder bool, index *tsi.MergeSetIndex) error
}

type TxtParser struct{}
type CsvParser struct{}

// Parse2SeriesKeyWithoutVersion parse encoded index key to line protocol series key,without version and escape special characters
// encoded index key format: [total len][ms len][ms][tagkey1 len][tagkey1 val]...]
// parse to line protocol format: mst,tagkey1=tagval1,tagkey2=tagval2...
func (T *TxtParser) Parse2SeriesKeyWithoutVersion(key []byte, dst []byte, splitWithNull bool) ([]byte, error) {
	msName, src, err := influx.MeasurementName(key)
	originMstName := influx.GetOriginMstName(string(msName))
	originMstName = EscapeMstName(originMstName)
	if err != nil {
		return []byte{}, err
	}
	var split [2]byte
	if splitWithNull {
		split[0], split[1] = influx.ByteSplit, influx.ByteSplit
	} else {
		split[0], split[1] = '=', ','
	}

	dst = append(dst, originMstName...)
	dst = append(dst, ',')
	tagsN := encoding.UnmarshalUint16(src)
	src = src[2:]
	var i uint16
	for i = 0; i < tagsN; i++ {
		keyLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		tagKey := EscapeTagKey(string(src[:keyLen]))
		dst = append(dst, tagKey...)
		dst = append(dst, split[0])
		src = src[keyLen:]

		valLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		tagVal := EscapeTagValue(string(src[:valLen]))
		dst = append(dst, tagVal...)
		dst = append(dst, split[1])
		src = src[valLen:]
	}
	return dst[:len(dst)-1], nil
}

func (T *TxtParser) AppendFields(rec record.Record, buf []byte) ([]byte, error) {
	for i, field := range rec.Schema {
		if field.Name == "time" {
			continue
		}
		buf = append(buf, EscapeFieldKey(field.Name)+"="...)
		switch field.Type {
		case influx.Field_Type_Float:
			buf = strconv.AppendFloat(buf, rec.Column(i).FloatValues()[0], 'g', -1, 64)
		case influx.Field_Type_Int:
			buf = strconv.AppendInt(buf, rec.Column(i).IntegerValues()[0], 10)
			buf = append(buf, 'i')
		case influx.Field_Type_Boolean:
			buf = strconv.AppendBool(buf, rec.Column(i).BooleanValues()[0])
		case influx.Field_Type_String:
			var str []string
			str = rec.Column(i).StringValues(str)
			buf = append(buf, '"')
			buf = append(buf, EscapeStringFieldValue(str[0])...)
			buf = append(buf, '"')
		default:
			// This shouldn't be possible, but we'll format it anyway.
			buf = append(buf, fmt.Sprintf("%v", rec.Column(i))...)
		}
		if i != rec.Len()-2 {
			buf = append(buf, ',')
		} else {
			buf = append(buf, ' ')
		}
	}
	buf = strconv.AppendInt(buf, rec.Times()[0], 10)
	buf = append(buf, '\n')
	return buf, nil
}

func (T *TxtParser) WriteMstInfo(metaWriter io.Writer, _ io.Writer, filePath string, isOrder bool, index *tsi.MergeSetIndex) error {
	lockPath := ""
	tsspFile, err := immutable.OpenTSSPFile(filePath, &lockPath, isOrder, false)
	defer util.MustClose(tsspFile)
	if err != nil {
		return err
	}
	fi := immutable.NewFileIterator(tsspFile, immutable.CLog)
	itr := immutable.NewChunkIterator(fi)
	itr.Next()
	sid := itr.GetSeriesID()
	if sid == 0 {
		return fmt.Errorf("series ID is zero")
	}
	rec := itr.GetRecord()
	record.CheckRecord(rec)
	var combineKey []byte
	var seriesKeys [][]byte
	var isExpectSeries []bool
	// Use sid get series key's []byte
	if seriesKeys, _, isExpectSeries, err = index.SearchSeriesWithTagArray(sid, seriesKeys, nil, combineKey, isExpectSeries, nil); err != nil {
		return err
	}
	_, src, err := influx.MeasurementName(seriesKeys[0])
	tagsN := encoding.UnmarshalUint16(src)
	src = src[2:]
	var i uint16
	var tags []string
	for i = 0; i < tagsN; i++ {
		keyLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		tagKey := EscapeTagKey(string(src[:keyLen]))
		tags = append(tags, tagKey)
		src = src[keyLen:]

		valLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		src = src[valLen:]
	}
	fmt.Fprintf(metaWriter, "# CONTEXT-TAGS: %s \n", strings.Join(tags, ","))
	return nil
}

// Parse2SeriesKeyWithoutVersion parse encoded index key to csv series key,without version and escape special characters
// encoded index key format: [total len][ms len][ms][tagkey1 len][tagkey1 val]...]
// parse to csv format: mst,tagval1,tagval2...
func (C *CsvParser) Parse2SeriesKeyWithoutVersion(key []byte, dst []byte, splitWithNull bool) ([]byte, error) {
	msName, src, err := influx.MeasurementName(key)
	originMstName := influx.GetOriginMstName(string(msName))
	originMstName = EscapeMstName(originMstName)
	if err != nil {
		return []byte{}, err
	}
	var split [2]byte
	if splitWithNull {
		split[0], split[1] = influx.ByteSplit, influx.ByteSplit
	} else {
		split[0], split[1] = '=', ','
	}

	tagsN := encoding.UnmarshalUint16(src)
	src = src[2:]
	var i uint16
	for i = 0; i < tagsN; i++ {
		keyLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		src = src[keyLen:]

		valLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		tagVal := EscapeTagValue(string(src[:valLen]))
		dst = append(dst, tagVal...)
		dst = append(dst, split[1])
		src = src[valLen:]
	}
	return dst, nil

}

func (C *CsvParser) AppendFields(rec record.Record, buf []byte) ([]byte, error) {
	for i, field := range rec.Schema {
		if field.Name == "time" {
			continue
		}
		switch field.Type {
		case influx.Field_Type_Float:
			buf = strconv.AppendFloat(buf, rec.Column(i).FloatValues()[0], 'g', -1, 64)
		case influx.Field_Type_Int:
			buf = strconv.AppendInt(buf, rec.Column(i).IntegerValues()[0], 10)
			buf = append(buf, 'i')
		case influx.Field_Type_Boolean:
			buf = strconv.AppendBool(buf, rec.Column(i).BooleanValues()[0])
		case influx.Field_Type_String:
			var str []string
			str = rec.Column(i).StringValues(str)
			buf = append(buf, '"')
			buf = append(buf, EscapeStringFieldValue(str[0])...)
			buf = append(buf, '"')
		default:
			// This shouldn't be possible, but we'll format it anyway.
			buf = append(buf, fmt.Sprintf("%v", rec.Column(i))...)
		}
		buf = append(buf, ',')
	}
	buf = strconv.AppendInt(buf, rec.Times()[0], 10)
	buf = append(buf, '\n')
	return buf, nil
}

func (C *CsvParser) WriteMstInfo(metaWriter io.Writer, outputWriter io.Writer, filePath string, isOrder bool, index *tsi.MergeSetIndex) error {
	lockPath := ""
	tsspFile, err := immutable.OpenTSSPFile(filePath, &lockPath, isOrder, false)
	defer util.MustClose(tsspFile)
	if err != nil {
		return err
	}
	fi := immutable.NewFileIterator(tsspFile, immutable.CLog)
	itr := immutable.NewChunkIterator(fi)
	itr.Next()
	sid := itr.GetSeriesID()
	if sid == 0 {
		return fmt.Errorf("series ID is zero")
	}
	rec := itr.GetRecord()
	record.CheckRecord(rec)
	var combineKey []byte
	var seriesKeys [][]byte
	var isExpectSeries []bool
	// Use sid get series key's []byte
	if seriesKeys, _, isExpectSeries, err = index.SearchSeriesWithTagArray(sid, seriesKeys, nil, combineKey, isExpectSeries, nil); err != nil {
		return err
	}
	_, src, err := influx.MeasurementName(seriesKeys[0])
	tagsN := encoding.UnmarshalUint16(src)
	src = src[2:]
	var i uint16
	var tags, fields []string
	for i = 0; i < tagsN; i++ {
		keyLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		tagKey := EscapeTagKey(string(src[:keyLen]))
		tags = append(tags, tagKey)
		src = src[keyLen:]

		valLen := encoding.UnmarshalUint16(src)
		src = src[2:]
		src = src[valLen:]
	}
	for _, field := range rec.Schema {
		fields = append(fields, field.Name)
	}
	fmt.Fprintf(metaWriter, "# CONTEXT-TAGS: %s \n", strings.Join(tags, ";"))
	buf := influx.GetBytesBuffer()
	buf = append(buf, strings.Join(tags, ",")...)
	buf = append(buf, ',')
	buf = append(buf, strings.Join(fields, ",")...)
	buf = append(buf, '\n')
	_, err = outputWriter.Write(buf)
	if err != nil {
		return err
	}
	return nil
}

func parseShardDir(shardDirName string) (uint64, uint64, error) {
	shardDir := strings.Split(shardDirName, dirNameSeparator)
	if len(shardDir) != 4 {
		return 0, 0, errno.NewError(errno.InvalidDataDir)
	}
	shardID, err := strconv.ParseUint(shardDir[0], 10, 64)
	if err != nil {
		return 0, 0, errno.NewError(errno.InvalidDataDir)
	}
	indexID, err := strconv.ParseUint(shardDir[3], 10, 64)
	if err != nil {
		return 0, 0, errno.NewError(errno.InvalidDataDir)
	}
	return shardID, indexID, nil
}

func parseIndexDir(indexDirName string) (uint64, error) {
	indexDir := strings.Split(indexDirName, dirNameSeparator)
	if len(indexDir) != 3 {
		return 0, errno.NewError(errno.InvalidDataDir)
	}

	indexID, err := strconv.ParseUint(indexDir[0], 10, 64)
	if err != nil {
		return 0, errno.NewError(errno.InvalidDataDir)
	}
	return indexID, nil
}

var escapeFieldKeyReplacer = strings.NewReplacer(`,`, `\,`, `=`, `\=`, ` `, `\ `)
var escapeTagKeyReplacer = strings.NewReplacer(`,`, `\,`, `=`, `\=`, ` `, `\ `)
var escapeTagValueReplacer = strings.NewReplacer(`,`, `\,`, `=`, `\=`, ` `, `\ `)
var escapeMstNameReplacer = strings.NewReplacer(`=`, `\=`, ` `, `\ `)
var escapeStringFieldReplacer = strings.NewReplacer(`"`, `\"`, `\`, `\\`)

// EscapeFieldKey returns a copy of in with any comma or equal sign or space
// with escaped values.
func EscapeFieldKey(in string) string {
	return escapeFieldKeyReplacer.Replace(in)
}

// EscapeStringFieldValue returns a copy of in with any double quotes or
// backslashes with escaped values.
func EscapeStringFieldValue(in string) string {
	return escapeStringFieldReplacer.Replace(in)
}

// EscapeTagKey returns a copy of in with any "comma" or "equal sign" or "space"
// with escaped values.
func EscapeTagKey(in string) string {
	return escapeTagKeyReplacer.Replace(in)
}

// EscapeTagValue returns a copy of in with any "comma" or "equal sign" or "space"
// with escaped values
func EscapeTagValue(in string) string {
	return escapeTagValueReplacer.Replace(in)
}

// EscapeMstName returns a copy of in with any "equal sign" or "space"
// with escaped values.
func EscapeMstName(in string) string {
	return escapeMstNameReplacer.Replace(in)
}
