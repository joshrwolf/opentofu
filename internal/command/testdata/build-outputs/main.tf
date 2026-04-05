resource "test_instance" "build" {
  ami = "build-image"
}

resource "test_instance" "test" {
  ami = test_instance.build.id
}

resource "test_instance" "tag" {
  ami = test_instance.test.id
}

output "image_ref" {
  value = test_instance.build.id
}

output "tags" {
  value = test_instance.tag.id
}
