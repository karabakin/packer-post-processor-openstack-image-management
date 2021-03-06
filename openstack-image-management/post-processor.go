//go:generate mapstructure-to-hcl2 -type Config

package openstackimagemanagement

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"sort"

	"github.com/gophercloud/gophercloud"
	gopenstack "github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/pagination"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer/builder/openstack"
	"github.com/hashicorp/packer/common"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

type Config struct {
	common.PackerConfig    `mapstructure:",squash"`
	openstack.AccessConfig `mapstructure:",squash"`

	Identifier   string `mapstructure:"identifier"`
	KeepReleases int    `mapstructure:"keep_releases"`

	ctx interpolate.Context
}

type OpenStackPostProcessor struct {
	config Config
	conn   *gophercloud.ServiceClient
}

func (p *OpenStackPostProcessor) ConfigSpec() hcldec.ObjectSpec {
	return p.config.FlatMapstructure().HCL2Spec()
}

func (p *OpenStackPostProcessor) Configure(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
	}, raws...)
	if err != nil {
		return err
	}

	var errs *packer.MultiError
	errs = packer.MultiErrorAppend(errs, p.config.AccessConfig.Prepare(&p.config.ctx)...)
	if len(errs.Errors) > 0 {
		return errs
	}

	log.Println(p.config)
	return nil
}

func (p *OpenStackPostProcessor) PostProcess(ctx context.Context, ui packer.Ui, artifact packer.Artifact) (packer.Artifact, bool, bool, error) {
	log.Println("Running OpenStack Image Management Post-Processor")

	if p.conn == nil {
		log.Println("Creating OpenStack connection")
		conn, err := p.imageV2Client()
		if err != nil {
			log.Println(err)
			return nil, true, false, err
		}
		p.conn = conn
	}

	var imageList []images.Image

	log.Println("Describing images for generation management")
	pager := images.List(p.conn, images.ListOpts{Name: p.config.Identifier})
	if err := pager.EachPage(func(page pagination.Page) (bool, error) {
		imgs, err := images.ExtractImages(page)
		if err != nil {
			return false, err
		}

		imageList = append(imageList, imgs...)
		return true, nil
	}); err != nil {
		return nil, true, false, err
	}

	sort.Slice(imageList, func(i, j int) bool {
		return imageList[i].CreatedAt.After(imageList[j].CreatedAt)
	})

	for i, img := range imageList {
		if i < p.config.KeepReleases {
			ui.Message(fmt.Sprintf("Updating meta for image: %s %s", img.Name, img.ID))
			updateOpts := images.UpdateOpts{
				images.UpdateImageProperty{
					Op:   images.RemoveOp,
					Name: "signature_verified",
				},
			}
			if result := images.Update(p.conn, img.ID, updateOpts); result.Err != nil {
				return nil, true, false, result.Err
			}
			continue
		}

		ui.Message(fmt.Sprintf("Deleting duplicating image: %s %s", img.Name, img.ID))
		log.Printf("Deleting duplicating image (%s) (%s)", img.Name, img.ID)
		if result := images.Delete(p.conn, img.ID); result.Err != nil {
			return nil, true, false, result.Err
		}
	}

	return artifact, true, false, nil
}

func (p *OpenStackPostProcessor) imageV2Client() (*gophercloud.ServiceClient, error) {
	opts := gophercloud.AuthOptions{
		IdentityEndpoint: p.config.IdentityEndpoint,
		UserID:           p.config.UserID,
		Username:         p.config.Username,
		Password:         p.config.Password,
		TenantID:         p.config.TenantID,
		TenantName:       p.config.TenantName,
		DomainID:         p.config.DomainID,
		DomainName:       p.config.DomainName,
		AllowReauth:      true,
	}

	client, err := gopenstack.NewClient(opts.IdentityEndpoint)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{}

	if p.config.CACertFile != "" {
		caCert, err := ioutil.ReadFile(p.config.CACertFile)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	if p.config.Insecure {
		tlsConfig.InsecureSkipVerify = true
	}

	if p.config.ClientCertFile != "" && p.config.ClientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(p.config.ClientCertFile, p.config.ClientKeyFile)
		if err != nil {
			return nil, err
		}

		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	transport := cleanhttp.DefaultTransport()
	transport.TLSClientConfig = tlsConfig
	client.HTTPClient.Transport = transport

	if err = gopenstack.Authenticate(client, opts); err != nil {
		return nil, err
	}

	return gopenstack.NewImageServiceV2(client, gophercloud.EndpointOpts{
		Region: p.config.Region,
	})
}
