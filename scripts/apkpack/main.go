// Command apkpack finishes the Android app's unsigned APK for
// scripts/build-android-app.sh: it copies everything aapt2 linked
// (manifest, resources, assets) exactly as stored — resources.arsc must
// stay uncompressed — and adds classes.dex and the native libraries
// (lib/<abi>/*.so). zipalign and apksigner run afterwards.
//
//	go run ./scripts/apkpack -in base.apk -dex classes.dex -lib build/lib -out unsigned.apk
package main

import (
	"archive/zip"
	"flag"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	in := flag.String("in", "", "the APK aapt2 linked")
	dex := flag.String("dex", "", "classes.dex from d8")
	lib := flag.String("lib", "", "directory holding <abi>/*.so")
	out := flag.String("out", "", "the APK to write (unaligned, unsigned)")
	flag.Parse()
	if *in == "" || *dex == "" || *out == "" {
		log.Fatal("apkpack: -in, -dex and -out are required")
	}
	zr, err := zip.OpenReader(*in)
	if err != nil {
		log.Fatalf("apkpack: %v", err)
	}
	defer zr.Close()
	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("apkpack: %v", err)
	}
	zw := zip.NewWriter(f)
	for _, e := range zr.File {
		// Keeps each entry's bytes and compression as aapt2 chose them, but
		// fixes the name: aapt2 on Windows stores asset paths with
		// backslashes ("assets\agents\x"), which Android can't find.
		fh := e.FileHeader
		fh.Name = strings.ReplaceAll(fh.Name, `\`, "/")
		w, err := zw.CreateRaw(&fh)
		if err != nil {
			log.Fatalf("apkpack: %s: %v", e.Name, err)
		}
		r, err := e.OpenRaw()
		if err == nil {
			_, err = io.Copy(w, r)
		}
		if err != nil {
			log.Fatalf("apkpack: copy %s: %v", e.Name, err)
		}
	}
	add := func(name, src string) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			log.Fatalf("apkpack: %v", err)
		}
		r, err := os.Open(src)
		if err != nil {
			log.Fatalf("apkpack: %v", err)
		}
		defer r.Close()
		if _, err := io.Copy(w, r); err != nil {
			log.Fatalf("apkpack: %s: %v", name, err)
		}
	}
	add("classes.dex", *dex)
	if *lib != "" {
		err := filepath.WalkDir(*lib, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".so") {
				return err
			}
			rel, _ := filepath.Rel(*lib, p)
			add("lib/"+filepath.ToSlash(rel), p)
			return nil
		})
		if err != nil {
			log.Fatalf("apkpack: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		log.Fatalf("apkpack: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Fatalf("apkpack: %v", err)
	}
}
