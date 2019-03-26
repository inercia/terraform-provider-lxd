package lxd

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxc/utils"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/i18n"
)

func resourceLxdBuiltImage() *schema.Resource {
	return &schema.Resource{
		Create: resourceLxdBuiltImageCreate,
		Update: resourceLxdBuiltImageUpdate,
		Delete: resourceLxdBuiltImageDelete,
		Exists: resourceLxdBuiltImageExists,
		Read:   resourceLxdBuiltImageRead,

		Schema: map[string]*schema.Schema{

			"template": {
				Type:     schema.TypeString,
				ForceNew: true,
				Required: true,
			},

			"remote": &schema.Schema{
				Type:     schema.TypeString,
				ForceNew: true,
				Optional: true,
				Default:  "",
			},

			"fingerprint": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"aliases": {
				Type:     schema.TypeList,
				ForceNew: false,
				Optional: true,
				Elem:     &schema.Schema{Type: schema.TypeString},
			},

			"created_at": {
				Type:     schema.TypeInt,
				Computed: true,
			},
		},
	}
}

func resourceLxdBuiltImageCreate(d *schema.ResourceData, meta interface{}) error {
	// create a temporary directory for the build
	dir, err := ioutil.TempDir("", "distrobuilder")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// create a distro definition file
	file, err := ioutil.TempFile(dir, "distrobuilder")
	if err != nil {
		return err
	}

	yaml_contents := d.Get("template")

	yaml := filepath.Join(dir, "distrobuilder.yaml")
	if err := ioutil.WriteFile(yaml, []byte(yaml_contents.(string)), 0644); err != nil {
		return err
	}
	defer os.Remove(file.Name())

	// run distrobuilder in the temporary directory
	cmd := exec.Command(
		"sudo",
		"distrobuilder",
		"build-lxd",
		yaml)

	cmd.Dir = dir

	cmdReader, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[ERROR] Error creating StdoutPipe for distrobuilder", err)
		return err
	}

	scanner := bufio.NewScanner(cmdReader)
	go func() {
		for scanner.Scan() {
			fmt.Printf("%s\n", scanner.Text())
		}
	}()

	err = cmd.Start()
	if err != nil {
		log.Printf("[ERROR] Error starting distrobuilder", err)
		return err
	}

	err = cmd.Wait()
	if err != nil {
		log.Printf("[ERROR] Error waiting for distrobuilder", err)
		return err
	}

	// at this moment, there should be a lxd.tar.xz file there
	meta_file := filepath.Join(dir, "lxd.tar.xz")
	if _, err := os.Stat(meta_file); os.IsNotExist(err) {
		log.Printf("[ERROR] lxd.tar.xz not found at %s", dir)
	}

	rootfs_file := filepath.Join(dir, "rootfs.squashfs")
	if _, err := os.Stat(meta_file); os.IsNotExist(err) {
		log.Printf("[ERROR] rootfs.squashfs not found at %s", dir)
	}

	// perform a `lxc image import lxd.tar.xz rootfs.squashfs --alias $(IMAGE_ALIAS)`
	p := meta.(*lxdProvider)
	dstName := p.selectRemote(d)
	dstServer, err := p.GetContainerServer(dstName)
	if err != nil {
		return err
	}

	// Get data about remote image, also checks it exists
	if fingerprint, ok := d.GetOk("fingerprint"); ok {
		imgInfo, _, err := dstServer.GetImage(fingerprint.(string))
		if err != nil {
			return err
		}

		log.Printf("[INFO] there is already an image with fingerprint %s in %s", fingerprint, dstName)
		log.Printf("[INFO] image info: %+v", imgInfo)

		// TODO: check if the image is already there, and if we should re-create the image or not
	}

	createArgs := &lxd.ImageCreateArgs{}
	image := api.ImagesPost{}

	aliases := make([]api.ImageAlias, 0)
	if v, ok := d.GetOk("aliases"); ok {
		for _, alias := range v.([]interface{}) {
			// Check image alias doesn't already exist on destination
			dstAliasTarget, _, _ := dstServer.GetImageAlias(alias.(string))
			if dstAliasTarget != nil {
				return fmt.Errorf("Image alias already exists on destination: %s", alias.(string))
			}

			ia := api.ImageAlias{
				Name: alias.(string),
			}

			aliases = append(aliases, ia)
		}
	}

	progress := utils.ProgressRenderer{
		Format: i18n.G("Transferring image: %s"),
		Quiet:  true,
	}

	var meta_reader io.ReadCloser
	var rootfs_reader io.ReadCloser

	meta, err = os.Open(meta_file)
	if err != nil {
		return err
	}
	defer meta_reader.Close()

	// Open rootfs
	rootfs_reader, err = os.Open(rootfs_file)
	if err != nil {
		return err
	}
	defer rootfs_reader.Close()

	createArgs = &lxd.ImageCreateArgs{
		MetaFile:        meta_reader,
		MetaName:        filepath.Base(meta_file),
		RootfsFile:      rootfs_reader,
		RootfsName:      filepath.Base(rootfs_file),
		ProgressHandler: progress.UpdateProgress,
	}
	image.Filename = createArgs.MetaName

	// Start the transfer
	op, err := dstServer.CreateImage(image, createArgs)
	if err != nil {
		progress.Done("")
		return err
	}

	// Wait for operation to finish
	err = utils.CancelableWait(op, &progress)
	if err != nil {
		progress.Done("")
		return err
	}
	opAPI := op.Get()

	// Get the fingerprint
	fingerprint := opAPI.Metadata["fingerprint"].(string)
	progress.Done(fmt.Sprintf(i18n.G("Image imported with fingerprint: %s"), fingerprint))

	// Add the aliases
	if len(aliases) > 0 {
		err = ensureImageAliases(dstServer, aliases, fingerprint)
		if err != nil {
			return err
		}
	}

	// Image was successfully copied, set resource ID
	id := newbuiltImageID(dstName, fingerprint)
	d.SetId(id.resourceID())

	return resourceLxdBuiltImageRead(d, meta)
}

func resourceLxdBuiltImageCopyProgressHandler(prog string) {
	log.Println("[DEBUG] - image copy progress: ", prog)
}

func resourceLxdBuiltImageUpdate(d *schema.ResourceData, meta interface{}) error {
	p := meta.(*lxdProvider)
	remote := p.selectRemote(d)
	server, err := p.GetContainerServer(remote)
	if err != nil {
		return err
	}
	id := newbuiltImageIDFromResourceID(d.Id())

	if d.HasChange("aliases") {
		old, new := d.GetChange("aliases")
		oldSet := schema.NewSet(schema.HashString, old.([]interface{}))
		newSet := schema.NewSet(schema.HashString, new.([]interface{}))
		aliasesToRemove := oldSet.Difference(newSet)
		aliasesToAdd := newSet.Difference(oldSet)

		// Delete removed
		for _, a := range aliasesToRemove.List() {
			alias := a.(string)
			err := server.DeleteImageAlias(alias)
			if err != nil {
				return err
			}
		}
		// Add new
		for _, a := range aliasesToAdd.List() {
			alias := a.(string)

			req := api.ImageAliasesPost{}
			req.Name = alias
			req.Target = id.fingerprint

			err := server.CreateImageAlias(req)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func resourceLxdBuiltImageDelete(d *schema.ResourceData, meta interface{}) error {
	p := meta.(*lxdProvider)
	remote := p.selectRemote(d)
	server, err := p.GetContainerServer(remote)
	if err != nil {
		return err
	}

	id := newbuiltImageIDFromResourceID(d.Id())

	op, err := server.DeleteImage(id.fingerprint)
	if err != nil {
		return err
	}

	return op.Wait()
}

func resourceLxdBuiltImageExists(d *schema.ResourceData, meta interface{}) (bool, error) {
	p := meta.(*lxdProvider)
	remote := p.selectRemote(d)
	server, err := p.GetContainerServer(remote)
	if err != nil {
		return false, err
	}

	id := newbuiltImageIDFromResourceID(d.Id())

	_, _, err = server.GetImage(id.fingerprint)
	if err != nil {
		if err.Error() == "not found" {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func resourceLxdBuiltImageRead(d *schema.ResourceData, meta interface{}) error {
	p := meta.(*lxdProvider)
	remote := p.selectRemote(d)
	server, err := p.GetImageServer(remote)
	if err != nil {
		return err
	}

	id := newbuiltImageIDFromResourceID(d.Id())

	img, _, err := server.GetImage(id.fingerprint)
	if err != nil {
		if err.Error() == "not found" {
			d.SetId("")
			return nil
		}
		return err
	}

	d.Set("fingerprint", id.fingerprint)
	d.Set("created_at", img.CreatedAt.Unix())

	// Read aliases from img and set in resource data
	// If the user has set 'copy_aliases' to true, then the
	// locally cached image will have aliases set that aren't
	// in the Terraform config.
	// These need to be filtered out here so not to cause a diff.
	var aliases []string
	copiedAliases := d.Get("copied_aliases").([]interface{})
	configAliases := d.Get("aliases").([]interface{})
	copiedSet := schema.NewSet(schema.HashString, copiedAliases)
	configSet := schema.NewSet(schema.HashString, configAliases)

	for _, a := range img.Aliases {
		if configSet.Contains(a.Name) || !copiedSet.Contains(a.Name) {
			aliases = append(aliases, a.Name)
		} else {
			log.Println("[DEBUG] filtered alias ", a)
		}
	}
	d.Set("aliases", aliases)

	return nil
}

type builtImageID struct {
	remote      string
	fingerprint string
}

func newbuiltImageID(remote, fingerprint string) builtImageID {
	return builtImageID{
		remote:      remote,
		fingerprint: fingerprint,
	}
}

func newbuiltImageIDFromResourceID(id string) builtImageID {
	parts := strings.SplitN(id, "/", 2)
	return builtImageID{
		remote:      parts[0],
		fingerprint: parts[1],
	}
}

func (id builtImageID) resourceID() string {
	return fmt.Sprintf("%s/%s", id.remote, id.fingerprint)
}

// Create the specified image alises, updating those that already exist
func ensureImageAliases(client lxd.ContainerServer, aliases []api.ImageAlias, fingerprint string) error {
	if len(aliases) == 0 {
		return nil
	}

	names := make([]string, len(aliases))
	for i, alias := range aliases {
		names[i] = alias.Name
	}
	sort.Strings(names)

	resp, err := client.GetImageAliases()
	if err != nil {
		return err
	}

	// Delete existing aliases that match provided ones
	for _, alias := range GetExistingAliases(names, resp) {
		err := client.DeleteImageAlias(alias.Name)
		if err != nil {
			fmt.Println(fmt.Sprintf(i18n.G("Failed to remove alias %s"), alias.Name))
		}
	}
	// Create new aliases
	for _, alias := range aliases {
		aliasPost := api.ImageAliasesPost{}
		aliasPost.Name = alias.Name
		aliasPost.Target = fingerprint
		err := client.CreateImageAlias(aliasPost)
		if err != nil {
			fmt.Println(fmt.Sprintf(i18n.G("Failed to create alias %s"), alias.Name))
		}
	}
	return nil
}

// GetExistingAliases returns the intersection between a list of aliases and all the existing ones.
func GetExistingAliases(aliases []string, allAliases []api.ImageAliasesEntry) []api.ImageAliasesEntry {
	existing := []api.ImageAliasesEntry{}
	for _, alias := range allAliases {
		name := alias.Name
		pos := sort.SearchStrings(aliases, name)
		if pos < len(aliases) && aliases[pos] == name {
			existing = append(existing, alias)
		}
	}
	return existing
}
