# "build" resource — no dependencies
resource "aws_instance" "build" {
  ami = "build-image"
}

# "test" resource — depends on build
resource "aws_instance" "test" {
  ami = aws_instance.build.id
}

# "tag" resource — depends on test (and transitively build)
resource "aws_instance" "tag" {
  ami = aws_instance.test.id
}

# Output that only needs the build resource
output "image_ref" {
  value = aws_instance.build.id
}

# Output that needs build + test + tag
output "tags" {
  value = aws_instance.tag.id
}
