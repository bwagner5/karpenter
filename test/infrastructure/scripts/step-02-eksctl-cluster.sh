cmd="create"
K8S_VERSION="1.22"
eksctl get cluster --name "${CLUSTER_NAME}" && cmd="upgrade"
eksctl ${cmd} cluster -f - <<EOF
---
apiVersion: eksctl.io/v1alpha5
kind: ClusterConfig
metadata:
  name: ${CLUSTER_NAME}
  region: ${AWS_REGION}
  version: "${K8S_VERSION}"
  tags:
    karpenter.sh/discovery: ${CLUSTER_NAME}
managedNodeGroups:
  - instanceTypes:
    - m5.large
    - m5a.large
    - m6i.large
    - c5.large
    - c5a.large
    - c6i.large
    amiFamily: AmazonLinux2
    name: ${CLUSTER_NAME}-system-pool
    desiredCapacity: 2
    minSize: 2
    maxSize: 2
    taints:
      - key: CriticalAddonsOnly
        value: "true"
        effect: NoSchedule
iam:
  withOIDC: true
  serviceAccounts:
  - metadata:
      name: aws-load-balancer-controller
      namespace: kube-system
    wellKnownPolicies:
      awsLoadBalancerController: true
    roleName: eksctl-awslb-role
  - metadata:
      name: tekton
      namespace: tekton-tests
    roleName: tekton-pods
    attachPolicy:
      Version: "2012-10-17"
      Statement:
        - Effect: Allow
      Resource: "*"
      Action:
        - ec2:*
        - cloudformation:*
        - iam:*
        - ssm:GetParameter
        - eks:*
addons:
  - name: vpc-cni
    version: 1.11.2
  - name: aws-ebs-csi-driver
  - name: kube-proxy
  - name: coredns
EOF

aws iam create-service-linked-role --aws-service-name spot.amazonaws.com || true

eksctl create iamidentitymapping --cluster "${CLUSTER_NAME}" \
  --arn "arn:aws:iam::${AWS_ACCOUNT_ID}:role/tekton-pods" \
  --group tekton \
  --username 'system:node:{{EC2PrivateDNSName}}'


