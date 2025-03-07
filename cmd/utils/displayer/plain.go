// Copyright 2022 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package displayer

import (
	"bufio"
	"context"
	"io"
	"os"
	"sync"

	oktetoLog "github.com/okteto/okteto/pkg/log"
)

type plainDisplayer struct {
	stdoutScanner *bufio.Scanner
	stderrScanner *bufio.Scanner

	commandContext context.Context
	cancel         context.CancelFunc
}

func newPlainDisplayer(stdout, stderr io.Reader) *plainDisplayer {
	var (
		stdoutScanner *bufio.Scanner
		stderrScanner *bufio.Scanner
	)
	if stdout != nil {
		stdoutScanner = bufio.NewScanner(stdout)
	}
	if stderr != nil {
		stderrScanner = bufio.NewScanner(stderr)
	}

	return &plainDisplayer{
		stdoutScanner: stdoutScanner,
		stderrScanner: stderrScanner,
	}
}

func (d *plainDisplayer) Display(_ string) {
	d.commandContext, d.cancel = context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wgDelta := 0
	if d.stdoutScanner != nil {
		wgDelta++
	}
	if d.stderrScanner != nil {
		wgDelta++
	}
	wg.Add(wgDelta)
	if d.stdoutScanner != nil {
		go func() {
			for d.stdoutScanner.Scan() {
				select {
				case <-d.commandContext.Done():
				default:
					line := d.stdoutScanner.Text()
					oktetoLog.FPrintln(os.Stdout, line)
					continue
				}
				break
			}
			wg.Done()
		}()
	}

	if d.stderrScanner != nil {
		go func() {
			for d.stderrScanner.Scan() {
				select {
				case <-d.commandContext.Done():
				default:
					line := d.stderrScanner.Text()
					oktetoLog.FWarning(os.Stdout, line)
					continue
				}
				break
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

// CleanUp stops displaying
func (d *plainDisplayer) CleanUp(_ error) {
	d.cancel()
	<-d.commandContext.Done()
}
