# A WireGuard hub in the same region as the sandboxes.
#
# Sandboxes dial out to the hub over the public internet, so the hub needs a
# public address and an open UDP range. Putting it in the sandboxes' own region
# keeps the overlay round trip inside one cloud.

terraform {
  required_version = ">= 1.5"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region  = var.region
  profile = var.profile
}

variable "region" {
  description = "Region to run the hub in. Match the region your sandboxes run in."
  type        = string
  default     = "us-east-1"
}

variable "profile" {
  description = "AWS CLI profile."
  type        = string
  default     = "ua-staging"
}

variable "name" {
  description = "Name prefix for every resource."
  type        = string
  default     = "simplecloud-hub"
}

variable "instance_type" {
  description = "WireGuard is CPU-bound on crypto, so pick for network bandwidth and clock speed."
  type        = string
  default     = "c7gn.large"
}

variable "public_key" {
  description = "SSH public key for the hub."
  type        = string
}

variable "ssh_cidr" {
  description = "Who may reach SSH. Defaults to nowhere, so set it to your own address."
  type        = string
  default     = "0.0.0.0/32"
}

variable "wireguard_ports" {
  description = "Inbound UDP range for project interfaces, matching the CLI's allocation."
  type = object({
    from = number
    to   = number
  })
  default = {
    from = 51820
    to   = 51899
  }
}

# Ubuntu 24.04 for the instance's architecture, chosen by the AMI owner rather
# than a pinned id, so this keeps working as images are republished.
data "aws_ec2_instance_type" "hub" {
  instance_type = var.instance_type
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd*/ubuntu-noble-24.04-*-server-*"]
  }

  filter {
    name   = "architecture"
    values = [contains(data.aws_ec2_instance_type.hub.supported_architectures, "arm64") ? "arm64" : "x86_64"]
  }
}

# Standard zones only. An account with Local Zones opted in would otherwise
# place the hub somewhere like us-east-1-atl-2a, which is a different city from
# the region proper and carries its own latency and instance catalogue.
data "aws_availability_zones" "available" {
  state = "available"

  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

resource "aws_vpc" "hub" {
  cidr_block           = "10.90.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = var.name }
}

resource "aws_internet_gateway" "hub" {
  vpc_id = aws_vpc.hub.id
  tags   = { Name = var.name }
}

resource "aws_subnet" "hub" {
  vpc_id                  = aws_vpc.hub.id
  cidr_block              = "10.90.1.0/24"
  availability_zone       = data.aws_availability_zones.available.names[0]
  map_public_ip_on_launch = true
  tags                    = { Name = var.name }
}

resource "aws_route_table" "hub" {
  vpc_id = aws_vpc.hub.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.hub.id
  }

  tags = { Name = var.name }
}

resource "aws_route_table_association" "hub" {
  subnet_id      = aws_subnet.hub.id
  route_table_id = aws_route_table.hub.id
}

resource "aws_security_group" "hub" {
  name        = var.name
  description = "WireGuard hub: SSH for the CLI, UDP for project interfaces"
  vpc_id      = aws_vpc.hub.id

  # The CLI drives the hub over SSH, so this only needs to reach the operator.
  ingress {
    description = "SSH from the operator"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_cidr]
  }

  # Sandboxes NAT out through addresses that differ per sandbox and change on
  # resume, so this range cannot be narrowed to known peers.
  ingress {
    description = "WireGuard from sandboxes"
    from_port   = var.wireguard_ports.from
    to_port     = var.wireguard_ports.to
    protocol    = "udp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Package installation during bootstrap"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = { Name = var.name }
}

resource "aws_key_pair" "hub" {
  key_name   = var.name
  public_key = var.public_key
  tags       = { Name = var.name }
}

resource "aws_instance" "hub" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.instance_type
  subnet_id              = aws_subnet.hub.id
  vpc_security_group_ids = [aws_security_group.hub.id]
  key_name               = aws_key_pair.hub.key_name

  # Nothing is installed here. `simplecloud hub add --bootstrap` takes a stock
  # image to a working hub, so this stays a plain instance.
  root_block_device {
    volume_size = 20
    volume_type = "gp3"
    encrypted   = true
  }

  metadata_options {
    http_tokens = "required"
  }

  tags = { Name = var.name }
}

output "hub" {
  description = "Pass this to the CLI as SIMPLECLOUD_HUB."
  value       = "ubuntu@${aws_instance.hub.public_ip}"
}

output "public_ip" {
  value = aws_instance.hub.public_ip
}

output "availability_zone" {
  value = aws_instance.hub.availability_zone
}

output "instance_type" {
  value = aws_instance.hub.instance_type
}

output "network_performance" {
  value = data.aws_ec2_instance_type.hub.network_performance
}
