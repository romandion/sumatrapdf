package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

/*
Some notes on the insanity that is setting up command-line build for both
32 and 64 bit executables.

Useful references:
https://msdn.microsoft.com/en-us/library/f2ccy3wt.aspx
https://msdn.microsoft.com/en-us/library/x4d2c09s.aspx
http://www.sqlite.org/src/artifact/60dbf6021d3de0a9 -sqlite's win build script

%VS140COMNTOOLS%\vsvars32.bat is how set basic env for 32bit builds for VS 2015
(it's VS120COMNTOOLS for VS 2013).

That sets VSINSTALLDIR env variable which we can use to setup both 32bit and
64bit builds:
%VCINSTALLDIR%\vcvarsall.bat x86_amd64 : 64bit
%VCINSTALLDIR%\vcvarsall.bat x86 : 32bit

If the OS is 64bit, there are also 64bit compilers that can be selected with:
amd64 (for 64bit builds) and amd64_x86 (for 32bit builds). They generate
the exact same code but can compiler bigger programs (can use more memory).

I'm guessing %VS140COMNTOOLS%\vsvars32.bat is the same as %VSINSTALLDIR%\vcvarsall.bat x86.
*/

type EnvVar struct {
	Name string
	Val  string
}

type Platform int
type Config int

const (
	Platform32Bit Platform = 1
	Platform64Bit Platform = 2

	ConfigDebug   Config = 1
	ConfigRelease Config = 2
	ConfigAnalyze Config = 3
)

var (
	alwaysRebuild bool = false
	wg            sync.WaitGroup
	sem           chan bool
)

// maps upper-cased name of env variable to Name/Val
func envToMap(env []string) map[string]*EnvVar {
	res := make(map[string]*EnvVar)
	for _, v := range env {
		if len(v) == 0 {
			continue
		}
		parts := strings.SplitN(v, "=", 2)
		if len(parts) != 2 {

		}
		nameUpper := strings.ToUpper(parts[0])
		res[nameUpper] = &EnvVar{
			Name: parts[0],
			Val:  parts[1],
		}
	}
	return res
}

func getEnvAfterScript(dir, script string) map[string]*EnvVar {
	// TODO: maybe use COMSPEC env variable instead of "cmd.exe" (more robust)
	cmd := exec.Command("cmd.exe", "/c", script+" & set")
	cmd.Dir = dir
	fmt.Printf("Executing: %s in %s\n", cmd.Args, cmd.Dir)
	resBytes, err := cmd.Output()
	if err != nil {
		fmt.Printf("failed with %s\n", err)
		os.Exit(1)
	}
	res := string(resBytes)
	//fmt.Printf("res:\n%s\n", res)
	parts := strings.Split(res, "\n")
	if len(parts) == 1 {
		fmt.Printf("split failed\n")
		fmt.Printf("res:\n%s\n", res)
		os.Exit(1)
	}
	for idx, env := range parts {
		env = strings.TrimSpace(env)
		parts[idx] = env
	}
	return envToMap(parts)
}

func calcEnvAdded(before, after map[string]*EnvVar) map[string]*EnvVar {
	res := make(map[string]*EnvVar)
	for k, afterVal := range after {
		beforeVal := before[k]
		if beforeVal == nil || beforeVal.Val != afterVal.Val {
			res[k] = afterVal
		}
	}
	return res
}

var (
	cachedVcInstallDir string
)

// return value of VCINSTALLDIR env variable after running vsvars32.bat
func getVcInstallDir(toolsDir string) string {
	if cachedVcInstallDir == "" {
		env := getEnvAfterScript(toolsDir, "vsvars32.bat")
		val := env["VCINSTALLDIR"]
		if val == nil {
			fmt.Printf("no 'VCINSTALLDIR' variable in %s\n", env)
			os.Exit(1)
		}
		cachedVcInstallDir = val.Val
	}
	return cachedVcInstallDir
}

func getEnvForVcTools(vcInstallDir, platform string) []string {
	//initialEnv := envToMap(os.Environ())
	afterEnv := getEnvAfterScript(vcInstallDir, "vcvarsall.bat "+platform)
	//return calcEnvAdded(initialEnv, afterEnv)

	var envArr []string
	for _, envVar := range afterEnv {
		v := fmt.Sprintf("%s=%s", envVar.Name, envVar.Val)
		envArr = append(envArr, v)
	}
	return envArr
}

func getEnv32(vcInstallDir string) []string {
	return getEnvForVcTools(vcInstallDir, "x86")
}

func getEnv64(vcInstallDir string) []string {
	return getEnvForVcTools(vcInstallDir, "x86_amd64")
}

func dumpEnv(env map[string]*EnvVar) {
	var keys []string
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := env[k]
		fmt.Printf("%s: %s\n", v.Name, v.Val)
	}
}

func getEnv(platform Platform) []string {
	initialEnv := envToMap(os.Environ())
	vs2013 := initialEnv["VS120COMNTOOLS"]
	vs2015 := initialEnv["VS140COMNTOOLS"]
	vsVar := vs2015
	if vsVar == nil {
		vsVar = vs2013
	}
	if vsVar == nil {
		fmt.Printf("VS120COMNTOOLS or VS140COMNTOOLS not set; VS 2013 or 2015 not installed\n")
		os.Exit(1)
	}
	vcInstallDir := getVcInstallDir(vsVar.Val)
	switch platform {
	case Platform32Bit:
		return getEnv32(vcInstallDir)
	case Platform64Bit:
		return getEnv64(vcInstallDir)
	default:
		panic("unknown platform")
	}
}

func getOutDir(platform Platform, config Config) string {
	dir := ""
	switch config {
	case ConfigRelease:
		dir = "rel"
	case ConfigDebug:
		dir = "dbg"
	}
	if platform == Platform64Bit {
		dir += "64"
	}
	return dir
}

type Args struct {
	args []string
}

func strConcat(arr1, arr2 []string) []string {
	var res []string
	for _, s := range arr1 {
		res = append(res, s)
	}
	for _, s := range arr2 {
		res = append(res, s)
	}
	return res
}

func (a *Args) Append(toAppend []string) *Args {
	return &Args{
		args: strConcat(a.args, toAppend),
	}
}

var (
	cachedExePaths map[string]string
	createdDirs    map[string]bool
	fileInfoCache  map[string]os.FileInfo
)

func init() {
	cachedExePaths = make(map[string]string)
	createdDirs = make(map[string]bool)
	fileInfoCache = make(map[string]os.FileInfo)
}

func fileExists(path string) bool {
	if _, ok := fileInfoCache[path]; !ok {
		fi, err := os.Stat(path)
		if err != nil {
			return false
		}
		fileInfoCache[path] = fi
	}
	fi := fileInfoCache[path]
	return fi.Mode().IsRegular()
}

func createDirCached(dir string) {
	if _, ok := createdDirs[dir]; ok {
		return
	}
	if err := os.MkdirAll(dir, 0644); err != nil {
		fatalf("os.MkdirAll(%s) failed wiht %s\n", dir, err)
	}
}

func getModTime(path string, def time.Time) time.Time {
	if _, ok := fileInfoCache[path]; !ok {
		fi, err := os.Stat(path)
		if err != nil {
			return def
		}
		fileInfoCache[path] = fi
	}
	fi := fileInfoCache[path]
	return fi.ModTime()
}

// returns true if dst doesn't exist or is older than src or any of the deps
func isOutdated(src, dst string, deps []string) bool {
	if alwaysRebuild {
		return true
	}
	if !fileExists(dst) {
		return true
	}
	dstTime := getModTime(dst, time.Now())
	srcTime := getModTime(src, time.Now())
	if srcTime.Sub(dstTime) > 0 {
		return true
	}
	for _, path := range deps {
		pathTime := getModTime(path, time.Now())
		if srcTime.Sub(pathTime) > 0 {
			return true
		}
	}
	if true {
		fmt.Printf("%s is up to date\n", dst)
	}
	return false
}

func createDirForFileCached(path string) {
	createDirCached(filepath.Dir(path))
}

func lookupInEnvPathUncached(exeName string, env []string) string {
	for _, envVar := range env {
		parts := strings.SplitN(envVar, "=", 2)
		name := strings.ToLower(parts[0])
		if name != "path" {
			continue
		}
		parts = strings.Split(parts[1], ";")
		for _, dir := range parts {
			path := filepath.Join(dir, exeName)
			if fileExists(path) {
				return path
			}
		}
		fatalf("didn't find %s in '%s'\n", exeName, parts[1])
	}
	return ""
}

func lookupInEnvPath(exeName string, env []string) string {
	if _, ok := cachedExePaths[exeName]; !ok {
		cachedExePaths[exeName] = lookupInEnvPathUncached(exeName, env)
		fmt.Printf("found %s as %s\n", exeName, cachedExePaths[exeName])
	}
	return cachedExePaths[exeName]
}

func runExeHelper(exeName string, env []string, args *Args) {
	exePath := lookupInEnvPath(exeName, env)
	cmd := exec.Command(exePath, args.args...)
	cmd.Env = env
	if true {
		args := cmd.Args
		args[0] = exeName
		fmt.Printf("Running %s\n", args)
		args[0] = exePath
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("%s failed with %s, out:\n%s\n", cmd.Args, err, string(out))
	}
}

func runExe(exeName string, env []string, args *Args) {
	semEnter()
	wg.Add(1)
	go func() {
		runExeHelper(exeName, env, args)
		semLeave()
		wg.Done()
	}()
}

func rc(src, dst string, env []string, args *Args) {
	createDirForFileCached(dst)
	extraArgs := []string{
		"/Fo" + dst,
		src,
	}
	args = args.Append(extraArgs)
	runExe("rc.exe", env, args)
}

func cl(src, dst string, env []string, args *Args) {
	if !isOutdated(src, dst, nil) {
		return
	}
	createDirForFileCached(dst)
	extraArgs := []string{
		"/Fo" + dst,
		src,
	}
	args = args.Append(extraArgs)
	runExe("cl.exe", env, args)
}

func fatalf(format string, args ...interface{}) {
	fmt.Printf(format, args...)
	os.Exit(1)
}

// given ${dir}/foo.rc, returns ${outDir}/${dir}/foo.rc
func rcOut(src, outDir string) string {
	verifyIsRcFile(src)
	s := filepath.Join(outDir, src)
	return replaceExt(s, ".res")
}

func verifyIsRcFile(path string) {
	s := strings.ToLower(path)
	if strings.HasSuffix(s, ".rc") {
		return
	}
	fatalf("%s should end in '.rc'\n", path)
}

func verifyIsCFile(path string) {
	s := strings.ToLower(path)
	if strings.HasSuffix(s, ".cpp") {
		return
	}
	if strings.HasSuffix(s, ".c") {
		return
	}
	fatalf("%s should end in '.c' or '.cpp'\n", path)
}

func replaceExt(path string, newExt string) string {
	ext := filepath.Ext(path)
	return path[0:len(path)-len(ext)] + newExt
}

func clOut(src, outDir string) string {
	verifyIsCFile(src)
	s := filepath.Join(outDir, src)
	return replaceExt(s, ".obj")
}

func clDir(srcDir string, files []string, outDir string, env []string, args *Args) {
	for _, f := range files {
		src := filepath.Join(srcDir, f)
		dst := clOut(src, outDir)
		cl(src, dst, env, args)
	}
}

func pj(elem ...string) string {
	return filepath.Join(elem...)
}

func build(platform Platform, config Config) {
	env := getEnv(platform)
	//dumpEnv(env)
	outDir := getOutDir(platform, config)
	createDirCached(outDir)

	rcArgs := []string{
		"/r",
		"/D", "DEBUG",
		"/D", "_DEBUG",
	}
	rcSrc := filepath.Join("src", "SumatraPDF.rc")
	rcDst := rcOut(rcSrc, outDir)
	rc(rcSrc, rcDst, env, &Args{args: rcArgs})

	startArgs := []string{
		"/nologo", "/c",
		"/D", "WIN32",
		"/D", "_WIN32",
		"/D", "WINVER=0x0501",
		"/D", "_WIN32_WINNT=0x0501",
		"/D", "DEBUG",
		"/D", "_DEBUG",
		"/D", "_USING_V110_SDK71_",
		"/GR-",
		"/Zi",
		"/GS",
		"/Gy",
		"/GF",
		"/arch:IA32",
		"/EHs-c-",
		"/MTd",
		"/Od",
		"/RTCs",
		"/RTCu",
		"/WX",
		"/W4",
		"/FS",
		"/wd4100",
		"/wd4127",
		"/wd4189",
		"/wd4428",
		"/wd4324",
		"/wd4458",
		"/wd4838",
		"/wd4800",
		"/Imupdf/include",
		"/Iext/zlib",
		"/Iext/lzma/C",
		"/Iext/libwebp",
		"/Iext/unarr",
		"/Iext/synctex",
		"/Iext/libdjvu",
		"/Iext/CHMLib/src",
		"/Isrc",
		"/Isrc/utils",
		"/Isrc/wingui",
		"/Isrc/mui",
		//fmt.Sprintf("/Fo%s\\sumatrapdf", outDir),
		fmt.Sprintf("/Fd%s\\vc80.pdb", outDir),
	}
	initialClArgs := &Args{
		args: startArgs,
	}
	srcFiles := []string{
		"AppPrefs.cpp",
		"DisplayModel.cpp",
		"CrashHandler.cpp",
		"Favorites.cpp",
		"TextSearch.cpp",
		"SumatraAbout.cpp",
		"SumatraAbout2.cpp",
		"SumatraDialogs.cpp",
		"SumatraProperties.cpp",
		"GlobalPrefs.cpp",
		"PdfSync.cpp",
		"RenderCache.cpp",
		"TextSelection.cpp",
		"WindowInfo.cpp",
		"ParseCOmmandLine.cpp",
		"StressTesting.cpp",
		"AppTools.cpp",
		"AppUtil.cpp",
		"TableOfContents.cpp",
		"Toolbar.cpp",
		"Print.cpp",
		"Notifications.cpp",
		"Selection.cpp",
		"Search.cpp",
		"ExternalViewers.cpp",
		"EbookControls.cpp",
		"EbookController.cpp",
		"Doc.cpp",
		"MuiEbookPageDef.cpp",
		"PagesLayoutDef.cpp",
		"Tester.cpp",
		"Translations.cpp",
		"Trans_sumatra_txt.cpp",
		"Tabs.cpp",
		"FileThumbnails.cpp",
		"FileHistory.cpp",
		"ChmModel.cpp",
		"Caption.cpp",
		"Canvas.cpp",
		"TabInfo.cpp",
	}
	clDir("src", srcFiles, outDir, env, initialClArgs)

	if false {
		regressFiles := []string{
			"Regress.cpp",
		}
		clDir(pj("src", "regress"), regressFiles, outDir, env, initialClArgs)
	}

	srcUtilsFiles := []string{
		"FileUtil.cpp",
		"HttpUtil.cpp",
		"StrUtil.cpp",
		"WinUtil.cpp",
		"GdiPlusUtil.cpp",
		"FileTransactions.cpp",
		"Touch.cpp",
		"TrivialHtmlParser.cpp",
		"HtmlWindow.cpp",
		"DirIter.cpp",
		"BitReader.cpp",
		"HtmlPullParser.cpp",
		"HtmlPrettyPrint.cpp",
		"ThreadUtil.cpp",
		"DebugLog.cpp",
		"DbgHelpDyn.cpp",
		"JsonParser.cpp",
		"TgaReader.cpp",
		"HtmlParserLookup.cpp",
		"ByteOrderDecoder.cpp",
		"CmdLineParser.cpp",
		"UITask.cpp",
		"StrFormat.cpp",
		"Dict.cpp",
		"BaseUtil.cpp",
		"CssParser.cpp",
		"FileWatcher.cpp",
		"CryptoUtil.cpp",
		"StrSlice.cpp",
		"TxtParser.cpp",
		"SerializeTxt.cpp",
		"SquareTreeParser.cpp",
		"SettingsUtil.cpp",
		"WebpReader.cpp",
		"FzImgReader.cpp",
		"ArchUtil.cpp",
		"ZipUtil.cpp",
		"LzmaSimpleArchive.cpp",
		"Dpi.cpp",
	}
	clDir(pj("src", "utils"), srcUtilsFiles, outDir, env, initialClArgs)
}

func semEnter() {
	sem <- true
}

func semLeave() {
	<-sem
}

func main() {
	n := runtime.NumCPU()
	fmt.Printf("Using %d goroutines\n", n)
	sem = make(chan bool, n)
	timeStart := time.Now()
	build(Platform32Bit, ConfigRelease)
	wg.Wait()
	fmt.Printf("total time: %s\n", time.Since(timeStart))
}
