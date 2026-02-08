package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var _ = Describe("Pod Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When reconciling a Shadow Service", func() {
		It("Should sync UDN IPs from Pods to the Shadow EndpointSlice", func() {
			ctx := context.Background()
			ns := "default"

			// 1. Create the TARGET Service (Original)
			targetName := "my-app-target"
			targetSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      targetName,
					Namespace: ns,
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": "my-app"},
					Ports: []corev1.ServicePort{
						{
							Name:       "http",
							Port:       80,
							TargetPort: intstr.FromInt(8080),
							Protocol:   corev1.ProtocolTCP,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, targetSvc)).To(Succeed())

			// 2. Create the SHADOW Service (The one with the annotation)
			shadowName := "my-app-shadow"
			shadowSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      shadowName,
					Namespace: ns,
					Annotations: map[string]string{
						"bizcochillo.io/shadow-of": targetName,
					},
				},
				Spec: corev1.ServiceSpec{
					ClusterIP: "None", // Headless
					// No Selector!
				},
			}
			Expect(k8sClient.Create(ctx, shadowSvc)).To(Succeed())

			// 3. Create a POD with the UDN Annotation
			podName := "my-app-0"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns,
					Labels:    map[string]string{"app": "my-app"}, // Matches Target Selector
					Annotations: map[string]string{
						// Simulating the Multus/CNI annotation
						"k8s.v1.cni.cncf.io/network-status": `[{
							"name": "udn-network",
							"interface": "net1",
							"ips": ["20.0.0.5"], 
							"default": true
						}]`,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			// 4. Manually set Pod Status to Running (EnvTest doesn't do this)
			pod.Status.Phase = corev1.PodRunning
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			// 5. ASSERT: Check if the EndpointSlice is created and populated
			sliceLookupKey := types.NamespacedName{Name: shadowName + "-udn", Namespace: ns}
			createdSlice := &discoveryv1.EndpointSlice{}

			Eventually(func() bool {
				err := k8sClient.Get(ctx, sliceLookupKey, createdSlice)
				if err != nil {
					return false
				}
				// Verify we have endpoints
				if len(createdSlice.Endpoints) == 0 {
					return false
				}
				// Verify it's the correct IP
				return createdSlice.Endpoints[0].Addresses[0] == "20.0.0.5"
			}, timeout, interval).Should(BeTrue())

			// 6. Verify Port Inheritance
			Expect(createdSlice.Ports).Should(HaveLen(1))
			Expect(*createdSlice.Ports[0].Port).Should(BeEquivalentTo(80))
		})
	})
})
