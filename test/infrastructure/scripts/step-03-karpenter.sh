echo "Installing Karpenter"

aws cloudformation deploy \
  --stack-name "KarpenterTestInfrastructure-${CLUSTER_NAME}" \
  --template-file ${SCRIPTPATH}/management-cluster.cloudformation.yaml \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides "ClusterName=${CLUSTER_NAME}"

ROLE="    - rolearn: arn:aws:iam::${AWS_ACCOUNT_ID}:role/KarpenterNodeRole-${CLUSTER_NAME}\n      username: system:node:{{EC2PrivateDNSName}}\n      groups:\n      - system:nodes\n      - system:bootstrappers"
kubectl get -n kube-system configmap/aws-auth -o yaml | awk "/mapRoles: \|/{print;print \"${ROLE}\";next}1" >/tmp/aws-auth-patch.yml
kubectl patch configmap/aws-auth -n kube-system --patch "$(cat /tmp/aws-auth-patch.yml)"

eksctl create iamserviceaccount \
  --cluster "${CLUSTER_NAME}" --name karpenter --namespace karpenter \
  --role-name "${CLUSTER_NAME}-karpenter" \
  --attach-policy-arn "arn:aws:iam::${AWS_ACCOUNT_ID}:policy/KarpenterControllerPolicy-${CLUSTER_NAME}" \
  --role-only \
  --approve

export CLUSTER_ENDPOINT=$(aws eks describe-cluster --name ${CLUSTER_NAME} --query "cluster.endpoint" --output json)

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: karpenter-helm-values
  namespace: karpenter
data:
  values.yaml: |-
    clusterName: ${CLUSTER_NAME}
    clusterEndpoint: ${CLUSTER_ENDPOINT}
    serviceAccount:
        annotations:
            eks.amazonaws.com/role-arn: arn:aws:iam::${AWS_ACCOUNT_ID}:role/${CLUSTER_NAME}-karpenter
    aws:
        defaultInstanceProfile: KarpenterNodeInstanceProfile-${CLUSTER_NAME}
    tolerations: 
    - key: CriticalAddonsOnly
      operator: Exists
EOF