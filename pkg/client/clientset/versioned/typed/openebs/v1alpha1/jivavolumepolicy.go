/*
Copyright 2021 The OpenEBS Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by client-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	"time"

	v1alpha1 "github.com/openebs/jiva-operator/pkg/apis/openebs/v1alpha1"
	scheme "github.com/openebs/jiva-operator/pkg/client/clientset/versioned/scheme"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
)

// JivaVolumePoliciesGetter has a method to return a JivaVolumePolicyInterface.
// A group's client should implement this interface.
type JivaVolumePoliciesGetter interface {
	JivaVolumePolicies(namespace string) JivaVolumePolicyInterface
}

// JivaVolumePolicyInterface has methods to work with JivaVolumePolicy resources.
type JivaVolumePolicyInterface interface {
	Create(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.CreateOptions) (*v1alpha1.JivaVolumePolicy, error)
	Update(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.UpdateOptions) (*v1alpha1.JivaVolumePolicy, error)
	UpdateStatus(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.UpdateOptions) (*v1alpha1.JivaVolumePolicy, error)
	Delete(ctx context.Context, name string, opts v1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error
	Get(ctx context.Context, name string, opts v1.GetOptions) (*v1alpha1.JivaVolumePolicy, error)
	List(ctx context.Context, opts v1.ListOptions) (*v1alpha1.JivaVolumePolicyList, error)
	Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.JivaVolumePolicy, err error)
	JivaVolumePolicyExpansion
}

// jivaVolumePolicies implements JivaVolumePolicyInterface
type jivaVolumePolicies struct {
	client rest.Interface
	ns     string
}

// newJivaVolumePolicies returns a JivaVolumePolicies
func newJivaVolumePolicies(c *OpenebsV1alpha1Client, namespace string) *jivaVolumePolicies {
	return &jivaVolumePolicies{
		client: c.RESTClient(),
		ns:     namespace,
	}
}

// Get takes name of the jivaVolumePolicy, and returns the corresponding jivaVolumePolicy object, and an error if there is any.
func (c *jivaVolumePolicies) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.JivaVolumePolicy, err error) {
	result = &v1alpha1.JivaVolumePolicy{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		Name(name).
		VersionedParams(&options, scheme.ParameterCodec).
		Do(ctx).
		Into(result)
	return
}

// List takes label and field selectors, and returns the list of JivaVolumePolicies that match those selectors.
func (c *jivaVolumePolicies) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.JivaVolumePolicyList, err error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	result = &v1alpha1.JivaVolumePolicyList{}
	err = c.client.Get().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Do(ctx).
		Into(result)
	return
}

// Watch returns a watch.Interface that watches the requested jivaVolumePolicies.
func (c *jivaVolumePolicies) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}
	opts.Watch = true
	return c.client.Get().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Timeout(timeout).
		Watch(ctx)
}

// Create takes the representation of a jivaVolumePolicy and creates it.  Returns the server's representation of the jivaVolumePolicy, and an error, if there is any.
func (c *jivaVolumePolicies) Create(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.CreateOptions) (result *v1alpha1.JivaVolumePolicy, err error) {
	result = &v1alpha1.JivaVolumePolicy{}
	err = c.client.Post().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(jivaVolumePolicy).
		Do(ctx).
		Into(result)
	return
}

// Update takes the representation of a jivaVolumePolicy and updates it. Returns the server's representation of the jivaVolumePolicy, and an error, if there is any.
func (c *jivaVolumePolicies) Update(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.UpdateOptions) (result *v1alpha1.JivaVolumePolicy, err error) {
	result = &v1alpha1.JivaVolumePolicy{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		Name(jivaVolumePolicy.Name).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(jivaVolumePolicy).
		Do(ctx).
		Into(result)
	return
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *jivaVolumePolicies) UpdateStatus(ctx context.Context, jivaVolumePolicy *v1alpha1.JivaVolumePolicy, opts v1.UpdateOptions) (result *v1alpha1.JivaVolumePolicy, err error) {
	result = &v1alpha1.JivaVolumePolicy{}
	err = c.client.Put().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		Name(jivaVolumePolicy.Name).
		SubResource("status").
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(jivaVolumePolicy).
		Do(ctx).
		Into(result)
	return
}

// Delete takes name of the jivaVolumePolicy and deletes it. Returns an error if one occurs.
func (c *jivaVolumePolicies) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	return c.client.Delete().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		Name(name).
		Body(&opts).
		Do(ctx).
		Error()
}

// DeleteCollection deletes a collection of objects.
func (c *jivaVolumePolicies) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	var timeout time.Duration
	if listOpts.TimeoutSeconds != nil {
		timeout = time.Duration(*listOpts.TimeoutSeconds) * time.Second
	}
	return c.client.Delete().
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		VersionedParams(&listOpts, scheme.ParameterCodec).
		Timeout(timeout).
		Body(&opts).
		Do(ctx).
		Error()
}

// Patch applies the patch and returns the patched jivaVolumePolicy.
func (c *jivaVolumePolicies) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.JivaVolumePolicy, err error) {
	result = &v1alpha1.JivaVolumePolicy{}
	err = c.client.Patch(pt).
		Namespace(c.ns).
		Resource("jivavolumepolicies").
		Name(name).
		SubResource(subresources...).
		VersionedParams(&opts, scheme.ParameterCodec).
		Body(data).
		Do(ctx).
		Into(result)
	return
}
