# Labels Validation 

# # Adding validation for nodepool

# ## checking for restricted labels while filtering out well known labels
for Version in  $(seq 0 1); do 
    yqVersion="$Version" yq eval '.spec.versions[env(yqVersion)].schema.openAPIV3Schema.properties.spec.properties.template.properties.metadata.properties.labels.x-kubernetes-validations += [
        {"message": "label domain \"karpenter.k8s.aws\" is restricted", "rule": "self.all(x, x in [\"karpenter.k8s.aws/instance-encryption-in-transit-supported\", \"karpenter.k8s.aws/instance-category\", \"karpenter.k8s.aws/instance-hypervisor\", \"karpenter.k8s.aws/instance-family\", \"karpenter.k8s.aws/instance-generation\", \"karpenter.k8s.aws/instance-local-nvme\", \"karpenter.k8s.aws/instance-size\", \"karpenter.k8s.aws/instance-cpu\",\"karpenter.k8s.aws/instance-memory\", \"karpenter.k8s.aws/instance-network-bandwidth\", \"karpenter.k8s.aws/instance-gpu-name\", \"karpenter.k8s.aws/instance-gpu-manufacturer\", \"karpenter.k8s.aws/instance-gpu-count\", \"karpenter.k8s.aws/instance-gpu-memory\", \"karpenter.k8s.aws/instance-accelerator-name\", \"karpenter.k8s.aws/instance-accelerator-manufacturer\", \"karpenter.k8s.aws/instance-accelerator-count\"] || !x.find(\"^([^/]+)\").endsWith(\"karpenter.k8s.aws\"))"}]' -i pkg/apis/crds/karpenter.sh_nodepools.yaml 
done