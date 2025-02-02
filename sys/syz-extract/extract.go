// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/syzkaller/pkg/ast"
	"github.com/google/syzkaller/pkg/compiler"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/tool"
	"github.com/google/syzkaller/sys/targets"
)

var (
	flagOS        = flag.String("os", runtime.GOOS, "target OS")
	flagBuild     = flag.Bool("build", false, "regenerate arch-specific kernel headers")
	flagSourceDir = flag.String("sourcedir", "", "path to kernel source checkout dir")
	flagIncludes  = flag.String("includedirs", "", "path to other kernel source include dirs separated by commas")
	flagBuildDir  = flag.String("builddir", "", "path to kernel build dir")
	flagArch      = flag.String("arch", "", "comma-separated list of arches to generate (all by default)")
)

type Arch struct {
	target      *targets.Target
	sourceDir   string
	includeDirs string
	buildDir    string
	build       bool
	files       []*File
	err         error
	done        chan bool
}

type File struct {
	arch       *Arch
	name       string
	consts     map[string]uint64
	undeclared map[string]bool
	info       *compiler.ConstInfo
	err        error
	done       chan bool
}

type Extractor interface {
	prepare(sourcedir string, build bool, arches []*Arch) error
	prepareArch(arch *Arch) error
	processFile(arch *Arch, info *compiler.ConstInfo) (map[string]uint64, map[string]bool, error)
}

var extractors = map[string]Extractor{
	targets.Windows: new(windows),
}

func main() {
	// 解析参数，主要是OS、arch、syzlang文件名
	flag.Parse()
	if *flagBuild && *flagBuildDir != "" {
		tool.Failf("-build and -builddir is an invalid combination")
	}
	OS := *flagOS
	extractor := extractors[OS]
	if extractor == nil {
		tool.Failf("unknown os: %v", OS)
	}
	// 根据OS、arch生成Arch结构体数组
	arches, err := createArches(OS, archList(OS, *flagArch), flag.Args())
	if err != nil {
		tool.Fail(err)
	}
	if *flagSourceDir == "" {
		tool.Fail(fmt.Errorf("provide path to kernel checkout via -sourcedir " +
			"flag (or make extract SOURCEDIR)"))
	}
	if err := extractor.prepare(*flagSourceDir, *flagBuild, arches); err != nil {
		tool.Fail(err)
	}

	jobC := make(chan interface{}, len(arches))
	for _, arch := range arches {
		jobC <- arch
	}
	// 对每种arch架构，多线程并发执行worker
	for p := 0; p < runtime.GOMAXPROCS(0); p++ {
		go worker(extractor, jobC)
	}

	failed := false
	constFiles := make(map[string]*compiler.ConstFile)
	// 这里采用了管道进行线程同步，worker函数中执行close操作后
	// 相应的管道将不再等待
	for _, arch := range arches {
		fmt.Printf("generating %v/%v...\n", OS, arch.target.Arch)
		// 这个语句会阻塞等待管道
		<-arch.done
		if arch.err != nil {
			failed = true
			fmt.Printf("%v\n", arch.err)
			continue
		}
		for _, f := range arch.files {
			<-f.done
			if f.err != nil {
				failed = true
				fmt.Printf("%v: %v\n", f.name, f.err)
				continue
			}
			if constFiles[f.name] == nil {
				constFiles[f.name] = compiler.NewConstFile()
			}
			constFiles[f.name].AddArch(f.arch.target.Arch, f.consts, f.undeclared)
		}
	}
	// 保存到相应的.const文件中
	for file, cf := range constFiles {
		outname := filepath.Join("sys", OS, file+".const")
		data := cf.Serialize()
		if len(data) == 0 {
			os.Remove(outname)
			continue
		}
		if err := osutil.WriteFile(outname, data); err != nil {
			tool.Failf("failed to write output file: %v", err)
		}
	}

	if !failed && *flagArch == "" {
		failed = checkUnsupportedCalls(arches)
	}
	for _, arch := range arches {
		if arch.build {
			os.RemoveAll(arch.buildDir)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func worker(extractor Extractor, jobC chan interface{}) {
	for job := range jobC {
		switch j := job.(type) {
		case *Arch:
			// 处理传入的extractor和arch结构体
			infos, err := processArch(extractor, j)
			j.err = err
			// 将管道关闭是为了通知main()函数go routine 某部分工作已经完成
			// 类似于使用信号量来保证线程同步
			close(j.done)
			if j.err == nil {
				for _, f := range j.files {
					f.info = infos[filepath.Join("sys", j.target.OS, f.name)]
					jobC <- f
				}
			}
		case *File:
			// 编译生成可执行文件，并搜集常量
			j.consts, j.undeclared, j.err = processFile(extractor, j.arch, j)
			close(j.done)
		}
	}
}

func createArches(OS string, archArray, files []string) ([]*Arch, error) {
	errBuf := new(bytes.Buffer)
	eh := func(pos ast.Pos, msg string) {
		fmt.Fprintf(errBuf, "%v: %v\n", pos, msg)
	}
	top := ast.ParseGlob(filepath.Join("sys", OS, "*.txt"), eh)
	if top == nil {
		return nil, fmt.Errorf("%v", errBuf.String())
	}
	allFiles := compiler.FileList(top, OS, eh)
	if allFiles == nil {
		return nil, fmt.Errorf("%v", errBuf.String())
	}
	var arches []*Arch
	for _, archStr := range archArray { // 遍历架构name数组
		buildDir := "" // 确定build文件夹路径
		if *flagBuild {
			dir, err := ioutil.TempDir("", "syzkaller-kernel-build")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp dir: %v", err)
			}
			buildDir = dir
		} else if *flagBuildDir != "" {
			buildDir = *flagBuildDir
		} else {
			buildDir = *flagSourceDir
		}
		// 获取targets.List中对应的OS和arch的Target结构体
		target := targets.Get(OS, archStr)
		if target == nil {
			return nil, fmt.Errorf("unknown arch: %v", archStr)
		}
		// 创建arch结构体
		arch := &Arch{
			target:      target,          // 存放特定OS以及arch的一些信息
			sourceDir:   *flagSourceDir,  // kernel source 路径
			includeDirs: *flagIncludes,   // kernel source header路径
			buildDir:    buildDir,        // build路径
			build:       *flagBuild,      // 是否重新生成架构指定的kernel header
			done:        make(chan bool), // 管道，用于 go routine间通信。当arch分析完成后，将会向该管道通知
		}
		archFiles := files
		if len(archFiles) == 0 {
			for file, meta := range allFiles {
				if meta.NoExtract || !meta.SupportsArch(archStr) {
					continue
				}
				archFiles = append(archFiles, file)
			}
		}
		sort.Strings(archFiles)
		for _, f := range archFiles { // 将syzlang文件名数组添加到arch结构体中
			arch.files = append(arch.files, &File{
				arch: arch,
				name: f,
				done: make(chan bool), // 当file 分析完成后，将会向该管道发出通知
			})
		}
		arches = append(arches, arch)
	}
	return arches, nil
}

// 确定待分析的目标架构，如果指定了架构则直接返回
// 如果未指定架构则返回所有架构的架构name数组
func archList(OS, arches string) []string {
	if arches != "" {
		return strings.Split(arches, ",")
	}
	var archArray []string
	for arch := range targets.List[OS] {
		archArray = append(archArray, arch)
	}
	sort.Strings(archArray)
	return archArray
}

func checkUnsupportedCalls(arches []*Arch) bool {
	supported := make(map[string]bool)
	unsupported := make(map[string]string)
	for _, arch := range arches {
		for _, f := range arch.files {
			for name := range f.consts {
				supported[name] = true
			}
			for name := range f.undeclared {
				unsupported[name] = f.name
			}
		}
	}
	failed := false
	for name, file := range unsupported {
		if supported[name] {
			continue
		}
		failed = true
		fmt.Printf("%v: %v is unsupported on all arches (typo?)\n",
			file, name)
	}
	return failed
}

func processArch(extractor Extractor, arch *Arch) (map[string]*compiler.ConstInfo, error) {
	errBuf := new(bytes.Buffer)
	eh := func(pos ast.Pos, msg string) { // [1] 定义错误处理函数
		fmt.Fprintf(errBuf, "%v: %v\n", pos, msg)
	}
	// [2] 将编写的txt文件解析成AST
	// top变量就是ast森林的根节点
	top := ast.ParseGlob(filepath.Join("sys", arch.target.OS, "*.txt"), eh)
	if top == nil {
		return nil, fmt.Errorf("%v", errBuf.String())
	}
	// [3] 从每个syzlang文件中提取出const值，返回syzlang文件名与其用到的常量数组的映射
	infos := compiler.ExtractConsts(top, arch.target, eh)
	if infos == nil {
		return nil, fmt.Errorf("%v", errBuf.String())
	}
	// [4] 补全某些arch的kern src可能会缺失的头文件
	if err := extractor.prepareArch(arch); err != nil {
		return nil, err
	}
	return infos, nil // [5] 将获取到的consts infos 返回给调用者
}

func processFile(extractor Extractor, arch *Arch, file *File) (map[string]uint64, map[string]bool, error) {
	inname := filepath.Join("sys", arch.target.OS, file.name)
	if file.info == nil {
		return nil, nil, fmt.Errorf("const info for input file %v is missing", inname)
	}
	if len(file.info.Consts) == 0 {
		return nil, nil, nil
	}
	return extractor.processFile(arch, file.info)
}
