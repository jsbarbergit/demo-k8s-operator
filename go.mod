module jsbarber.net/vpce

go 1.15

require (
	github.com/aws/aws-sdk-go v1.38.37
	github.com/aws/aws-sdk-go-v2 v1.4.0
	github.com/aws/aws-sdk-go-v2/config v1.1.7
	github.com/aws/aws-sdk-go-v2/service/ec2 v1.6.0
	github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2 v1.2.3
	github.com/aws/aws-sdk-go-v2/service/s3 v1.6.0
	github.com/ericchiang/k8s v1.2.0 // indirect
	github.com/go-logr/logr v0.3.0
	github.com/google/martian v2.1.0+incompatible
	github.com/google/uuid v1.2.0
	github.com/onsi/ginkgo v1.14.1
	github.com/onsi/gomega v1.10.2
	k8s.io/api v0.19.2
	k8s.io/apimachinery v0.19.2
	k8s.io/client-go v0.19.2
	sigs.k8s.io/controller-runtime v0.7.2
)
