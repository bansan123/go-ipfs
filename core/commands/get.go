package commands

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	gopath "path"
	"strings"

	core "github.com/ipfs/go-ipfs/core"
	e "github.com/ipfs/go-ipfs/core/commands/e"
	dag "github.com/ipfs/go-ipfs/merkledag"
	path "github.com/ipfs/go-ipfs/path"
	tar "github.com/ipfs/go-ipfs/thirdparty/tar"
	uarchive "github.com/ipfs/go-ipfs/unixfs/archive"

	"gx/ipfs/QmPMeikDc7tQEDvaS66j1bVPQ2jBkvFwz3Qom5eA5i4xip/go-ipfs-cmds"
	"gx/ipfs/QmPhtZyjPYddJ8yGPWreisp47H6iQjt3Lg8sZrzqMP5noy/go-ipfs-cmds"
	"gx/ipfs/QmeWjRodbcZFKe5tMN7poEx3izym6osrLSnTLf9UjJZBbs/pb"
)

var ErrInvalidCompressionLevel = errors.New("Compression level must be between 1 and 9")

var GetCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Download IPFS objects.",
		ShortDescription: `
Stores to disk the data contained an IPFS or IPNS object(s) at the given path.

By default, the output will be stored at './<ipfs-path>', but an alternate
path can be specified with '--output=<path>' or '-o=<path>'.

To output a TAR archive instead of unpacked files, use '--archive' or '-a'.

To compress the output with GZIP compression, use '--compress' or '-C'. You
may also specify the level of compression by specifying '-l=<1-9>'.
`,
	},

	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, false, "The path to the IPFS object(s) to be outputted.").EnableStdin(),
	},
	Options: []cmds.Option{
		cmds.StringOption("output", "o", "The path where the output should be stored."),
		cmds.BoolOption("archive", "a", "Output a TAR archive.").Default(false),
		cmds.BoolOption("compress", "C", "Compress the output with GZIP compression.").Default(false),
		cmds.IntOption("compression-level", "l", "The level of compression (1-9).").Default(-1),
	},
	PreRun: func(req cmds.Request) error {
		_, err := getCompressOptions(req)
		return err
	},
	Run: func(req cmds.Request, res cmds.Response) {
		if len(req.Arguments()) == 0 {
			re.SetError(errors.New("not enough arugments provided"), cmds.ErrClient)
			return
		}
		cmplvl, err := getCompressOptions(req)
		if err != nil {
			re.SetError(err, cmds.ErrNormal)
			return
		}

		node, err := req.InvocContext().GetNode()
		if err != nil {
			re.SetError(err, cmds.ErrNormal)
			return
		}
		p := path.Path(req.Arguments()[0])
		ctx := req.Context()
		dn, err := core.Resolve(ctx, node.Namesys, node.Resolver, p)
		if err != nil {
			re.SetError(err, cmds.ErrNormal)
			return
		}

		switch dn := dn.(type) {
		case *dag.ProtoNode:
			size, err := dn.Size()
			if err != nil {
				re.SetError(err, cmds.ErrNormal)
				return
			}

			re.SetLength(size)
		case *dag.RawNode:
			re.SetLength(uint64(len(dn.RawData())))
		default:
			re.SetError(err, cmds.ErrNormal)
			return
		}

		archive, _, _ := req.Option("archive").Bool()
		reader, err := uarchive.DagArchive(ctx, dn, p.String(), node.DAG, archive, cmplvl)
		if err != nil {
			re.SetError(err, cmds.ErrNormal)
			return
		}

		re.Emit(reader)
	},
	PostRun: map[cmds.EncodingType]func(cmds.Request,s cmds.Response) cmds.ResponseEmitter{
		cmds.CLI: func(req cmds.Request, res cmds.Response) cmds.ResponseEmitter {
			reNext, res := cmds.NewChanResponsePair(req)

			go func() {
				defer re.Close()

				v, err := res.Next()
				if err != nil {
					log.Error(e.New(err))
					return
				}

				outReader, ok := v.(io.Reader)
				if !ok {
					log.Error(e.New(e.TypeErr(outReader, v)))
					return
				}

				outPath, _, _ := req.Option("output").String()
				if len(outPath) == 0 {
					_, outPath = gopath.Split(req.Arguments()[0])
					outPath = gopath.Clean(outPath)
				}

				cmplvl, err := getCompressOptions(req)
				if err != nil {
					re.SetError(err, cmds.ErrNormal)
					return
				}

				archive, _, _ := req.Option("archive").Bool()

				gw := getWriter{
					Out:         os.Stdout,
					Err:         os.Stderr,
					Archive:     archive,
					Compression: cmplvl,
					Size:        int64(res.Length()),
				}

				if err := gw.Write(outReader, outPath); err != nil {
					re.SetError(err, cmds.ErrNormal)
				}
			}()

			return reNext
		},
	},
}

type clearlineReader struct {
	io.Reader
	out io.Writer
}

func (r *clearlineReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	if err == io.EOF {
		// callback
		fmt.Fprintf(r.out, "\033[2K\r") // clear progress bar line on EOF
	}
	return
}

func progressBarForReader(out io.Writer, r io.Reader, l int64) (*pb.ProgressBar, io.Reader) {
	bar := makeProgressBar(out, l)
	barR := bar.NewProxyReader(r)
	return bar, &clearlineReader{barR, out}
}

func makeProgressBar(out io.Writer, l int64) *pb.ProgressBar {
	// setup bar reader
	// TODO: get total length of files
	bar := pb.New64(l).SetUnits(pb.U_BYTES)
	bar.Output = out

	// the progress bar lib doesn't give us a way to get the width of the output,
	// so as a hack we just use a callback to measure the output, then git rid of it
	bar.Callback = func(line string) {
		terminalWidth := len(line)
		bar.Callback = nil
		log.Infof("terminal width: %v\n", terminalWidth)
	}
	return bar
}

type getWriter struct {
	Out io.Writer // for output to user
	Err io.Writer // for progress bar output

	Archive     bool
	Compression int
	Size        int64
}

func (gw *getWriter) Write(r io.Reader, fpath string) error {
	if gw.Archive || gw.Compression != gzip.NoCompression {
		return gw.writeArchive(r, fpath)
	}
	return gw.writeExtracted(r, fpath)
}

func (gw *getWriter) writeArchive(r io.Reader, fpath string) error {
	// adjust file name if tar
	if gw.Archive {
		if !strings.HasSuffix(fpath, ".tar") && !strings.HasSuffix(fpath, ".tar.gz") {
			fpath += ".tar"
		}
	}

	// adjust file name if gz
	if gw.Compression != gzip.NoCompression {
		if !strings.HasSuffix(fpath, ".gz") {
			fpath += ".gz"
		}
	}

	// create file
	file, err := os.Create(fpath)
	if err != nil {
		return err
	}
	defer file.Close()

	fmt.Fprintf(gw.Out, "Saving archive to %s\n", fpath)
	bar, barR := progressBarForReader(gw.Err, r, gw.Size)
	bar.Start()
	defer bar.Finish()

	_, err = io.Copy(file, barR)
	return err
}

func (gw *getWriter) writeExtracted(r io.Reader, fpath string) error {
	fmt.Fprintf(gw.Out, "Saving file(s) to %s\n", fpath)
	bar := makeProgressBar(gw.Err, gw.Size)
	bar.Start()
	defer bar.Finish()
	defer bar.Set64(gw.Size)

	extractor := &tar.Extractor{fpath, bar.Add64}
	return extractor.Extract(r)
}

func getCompressOptions(req cmds.Request) (int, error) {
	cmprs, _, _ := req.Option("compress").Bool()
	cmplvl, cmplvlFound, _ := req.Option("compression-level").Int()
	switch {
	case !cmprs:
		return gzip.NoCompression, nil
	case cmprs && !cmplvlFound:
		return gzip.DefaultCompression, nil
	case cmprs && cmplvlFound && (cmplvl < 1 || cmplvl > 9):
		return gzip.NoCompression, ErrInvalidCompressionLevel
	}
	return cmplvl, nil
}
