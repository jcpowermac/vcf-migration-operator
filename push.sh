#!/bin/bash
export KUBECONFIG=${HOME}/Development/before-installer-testing/vsphere-ipi/auth/kubeconfig

make operator-image operator-push IMG=quay.io/jcallen/vcf-migration-operator:latest

oc get nodes
read -p "Press Enter to continue..."

make deploy IMG=quay.io/jcallen/vcf-migration-operator:latest
