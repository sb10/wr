// Copyright © 2017 Genome Research Limited
// Author: Sendu Bala <sb10@sanger.ac.uk>.
//
//  This file is part of wr.
//
//  wr is free software: you can redistribute it and/or modify
//  it under the terms of the GNU Lesser General Public License as published by
//  the Free Software Foundation, either version 3 of the License, or
//  (at your option) any later version.
//
//  wr is distributed in the hope that it will be useful,
//  but WITHOUT ANY WARRANTY; without even the implied warranty of
//  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//  GNU Lesser General Public License for more details.
//
//  You should have received a copy of the GNU Lesser General Public License
//  along with wr. If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"encoding/json"
	"github.com/VertebrateResequencing/wr/minfys"
	"github.com/spf13/cobra"
)

// options for this cmd
var mountJSON string

// mountCmd represents the mount command
var mountCmd = &cobra.Command{
	Use:   "mount",
	Short: "Mount an S3 bucket",
	Long: `Test mounting of S3 buckets.

'wr add' can take mount options if your commands need to read from/ write to
S3 buckets. Before supplying these mount options to 'wr add', you can use this
command to test that your mount options work.

You can also use this as a quick, easy and high performance way of mounting an
S3 bucket for general use, but note that it is only designed as a temporary
mount since it won't notice externally altered or added files in directories you
already accessed. It also only allows yourself access to the files.

Since this command doesn't run as a daemon, you'll have to either keep it
running in the foreground and open up a new terminal to actually explore your
mount point, or you'll have to run this command in the background:
$ wr mount -m '...' &
When you're finished with your mount, you must either send SIGTERM to its
process id (using the 'kill' command) or bring 'wr mount' back to the
foreground:
$ fg
And then kill it by hitting ctrl-c.
NB: if you are writing to your mount point, it's very important to kill it
cleanly using one of these methods once you're done, since uploads only occur
when you do this!

The --mount option is the JSON string for an array of Config objects describing
all your mount parameters.
A JSON array begins and ends with a square bracket, and each item is separated
with a comma.
A JSON object can be written by starting and ending it with curly braces.
Parameter names and their values are put in double quotes (except for numbers,
which are left bare, booleans where you write, unquoted, true or false, and
arrays as described previously), and the pair separated with a colon, and pairs
separated from each other with commas.
For example (all on one line): --mount '[{"Mount":"/tmp/wr_mnt","Verbose":true,
"Targets":[{"Profile":"default","Path":"mybucket/subdir","Cache":true,"Write":
true}]}]'
The paragraphs below describe all the possible Config object parameters.

Mount (required) is the local directory on which to mount the remote Path. It
can be (in) any directory you're able to write to. If the directory doesn't
exist, wr will try to create it first.

CacheBase is the parent directory to use for the CacheDir of any Targets
configured with Cache on, but CacheDir undefined. If CacheBase is also
undefined, the cache directories will be made in the current working directory.

Retries is the number of retries wr should attempt when it encounters errors in
trying to access your remote S3 bucket. At least 3 is recommended. It defaults
to 10 if not provided.

Verbose is a boolean, which if true, makes this command log timing messages for
every remote operation it carries out. It always logs errors.

Targets is an array of Target objects which define what you want to access at
your Mount. It's an array to allow you to multiplex different buckets (or
different subdirectories of the same bucket) so that it looks like all their
data is in the same place, for easier access to files in your mount. You can
only have one of these configured to be writeable. (If you don't want to
multiplex but instead want multiple different mount points, you specify a single
Target in this array, and have multiple Config objects in your top level array.)
The remaining paragraphs describe the possible parameters for Target objects.

Profile is the S3 configuration profile name to use. If not supplied, the value
of the $AWS_DEFAULT_PROFILE or $AWS_PROFILE environment variables is used, and
if those are unset it defaults to "default".
wr will look at a number of standard S3 configuration files and environment
variables to determine the scheme, domain, region and authentication details to
connect to S3 with. All possible sources are checked to fill in any missing
values from more preferred sources.
The preferred file is ~/.s3cfg, since this is the only config file type that
allows the specification of a custom domain. This file is Amazon's s3cmd config
file, described here: http://s3tools.org/kb/item14.htm. wr will look at the
access_key, secret_key, use_https and host_base options under the section with
the given Profile name. If you don't wish to use any other config files or
environment variables, you can add the non-standard region option to this file
if you need to specify a specific region.
The next file checked is the one pointed to by the $AWS_SHARED_CREDENTIALS_FILE
environment variable, or ~/.aws/credentials. This file is described here:
http://docs.aws.amazon.com/cli/latest/userguide/cli-chap-getting-started.html.
wr will look at the aws_access_key_id and aws_secret_access_key options under
the section with the given Profile name.
wr also checks the file pointed to by the $AWS_CONFIG_FILE environment variable,
or ~/.aws/config, described in the previous link. From here the region option is
used from the section with the given Profile name. If you don't wish to use a
~/.s3cfg file but do need to specify a custom domain, you can add the
non-standard host_base and use_https options to this file instead.
As a last resort, ~/.awssecret is checked. This is s3fs's config file, and
consists of a single line with your access key and secret key separated by a
colon.
If set, the environment variables $AWS_ACCESS_KEY_ID, $AWS_SECRET_ACCESS_KEY and
$AWS_DEFAULT_REGION override corresponding options found in any config file.

Path (required) is the name of your S3 bucket, optionally followed URL-style
(separated with forward slashes) by sub-directory names. The highest performance
is gained by specifying the deepest path under your bucket that holds all the
files you wish to access.

Cache is a boolean, which if true, turns on data caching of any data retrieved,
or any data you wish to upload. Caching is currently REQUIRED if you wish to do
any write operations.

CacheDir is the local directory to store cached data. If this parameter is
supplied, Cache is forced true and so doesn't need to be provided. If this
parameter is not supplied but Cache is true, the directory will be a unique
directory in CacheBase, which will get deleted on unmount.

Write is a boolean, which if true, makes the mount point writeable. If you
don't intend to write to a mount, just leave this parameter out. Because writing
currently requires caching, turning this on forces Cache to be true.`,
	Run: func(cmd *cobra.Command, args []string) {
		if mountJSON == "" {
			die("--mount is required")
		}

		for _, cfg := range mountParseJson(mountJSON) {
			fs, err := minfys.New(cfg)
			if err != nil {
				die("bad configuration: %s\n", err)
			}

			err = fs.Mount()
			if err != nil {
				die("could not mount: %s\n", err)
			}
			fs.UnmountOnDeath()
		}

		// wait forever
		select {}
	},
}

func init() {
	RootCmd.AddCommand(mountCmd)

	// flags specific to this sub-command
	mountCmd.Flags().StringVarP(&mountJSON, "mount", "m", "", "mount parameters JSON (see --help)")
}

// mountConfObj is the struct for the objects in the user's --mount JSON array.
type mountConfObj struct {
	Mount     string
	CacheBase string
	Retries   int
	Verbose   bool
	Targets   []mountTargetObj
}

// mountTargetObj is the struct for the objects in mountConfObj's Targets array.
type mountTargetObj struct {
	Profile  string
	Path     string
	Cache    bool
	CacheDir string
	Write    bool
}

// mountParseJson takes a json string (as per `wr mount --help`) and generates a
// minfys Config from it for each mount configured.
func mountParseJson(jsonString string) (configs []*minfys.Config) {
	var ml []mountConfObj
	err := json.Unmarshal([]byte(jsonString), &ml)
	if err != nil {
		die("had a problem with the provided mount JSON (%s): %s", jsonString, err)
	}

	for _, mj := range ml {
		if mj.Mount == "" {
			die("had a problem with the provided mount JSON (%s): missing Mount", jsonString)
		}

		var targets []*minfys.Target
		for _, mt := range mj.Targets {
			target := &minfys.Target{
				CacheData: mt.Cache,
				CacheDir:  mt.CacheDir,
				Write:     mt.Write,
			}
			err = target.ReadEnvironment(mt.Profile, mt.Path)
			if err != nil {
				die("had a problem reading S3 config values: %s", err)
			}
			targets = append(targets, target)
		}

		if len(targets) == 0 {
			die("had a problem with the provided mount JSON (%s): no Targets", jsonString)
		}

		retries := 10
		if mj.Retries > 0 {
			retries = mj.Retries
		}

		cfg := &minfys.Config{
			Mount:     mj.Mount,
			CacheBase: mj.CacheBase,
			Retries:   retries,
			Verbose:   mj.Verbose,
			Quiet:     false,
			Targets:   targets,
		}

		configs = append(configs, cfg)
	}

	return
}
