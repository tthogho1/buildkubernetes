FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive
ENV TERRAFORM_VERSION=1.9.8

# Base tools
RUN apt-get update && apt-get install -y \
    curl \
    unzip \
    git \
    python3 \
    python3-pip \
    openssh-client \
    gnupg \
    software-properties-common \
    sshpass \
    && rm -rf /var/lib/apt/lists/*

# Install Terraform
RUN curl -fsSL "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_amd64.zip" -o terraform.zip \
    && unzip terraform.zip -d /usr/local/bin \
    && rm terraform.zip \
    && terraform version

# Install Ansible + AWS SDK for Python (needed by some ansible aws modules)
RUN pip3 install --no-cache-dir ansible boto3 botocore

# Install AWS CLI v2
RUN curl -fsSL "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o awscliv2.zip \
    && unzip awscliv2.zip \
    && ./aws/install \
    && rm -rf awscliv2.zip aws

WORKDIR /workspace

CMD ["/bin/bash"]
