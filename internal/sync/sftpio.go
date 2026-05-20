package sync

import (
	"io"
	"os"

	"github.com/pkg/sftp"
)

func uploadFileSFTP(cli *sftp.Client, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := cli.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func downloadFileSFTP(cli *sftp.Client, src, dst string) error {
	in, err := cli.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
