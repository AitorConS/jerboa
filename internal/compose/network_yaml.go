package compose

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Accept both the short network list and Compose's attachment mapping. The
// runtime has one NIC; reject additional attachments rather than ignoring them.
func (s *Service) UnmarshalYAML(n *yaml.Node) error {
	type plain Service
	clone := *n
	clone.Content = append([]*yaml.Node(nil), n.Content...)
	for i := 0; i+1 < len(clone.Content); i += 2 {
		if clone.Content[i].Value != "networks" || clone.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		attachments := clone.Content[i+1]
		list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for j := 0; j+1 < len(attachments.Content); j += 2 {
			list.Content = append(list.Content, attachments.Content[j])
			var a struct {
				Aliases []string `yaml:"aliases"`
				IP      string   `yaml:"ipv4_address"`
			}
			if err := attachments.Content[j+1].Decode(&a); err != nil {
				return fmt.Errorf("compose network YAML: %w", err)
			}
			if a.IP != "" {
				clone.Content = append(clone.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "ip"}, &yaml.Node{Kind: yaml.ScalarNode, Value: a.IP})
			}
			if len(a.Aliases) > 0 {
				v := &yaml.Node{}
				if err := v.Encode(a.Aliases); err != nil {
					return fmt.Errorf("compose network YAML: %w", err)
				}
				clone.Content = append(clone.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "aliases"}, v)
			}
		}
		clone.Content[i+1] = list
	}
	if err := clone.Decode((*plain)(s)); err != nil {
		return fmt.Errorf("decode service networks: %w", err)
	}
	return nil
}
func (n *Network) UnmarshalYAML(node *yaml.Node) error {
	type plain Network
	var value struct {
		plain `yaml:",inline"`
		IPAM  struct {
			Config []struct {
				Subnet  string `yaml:"subnet"`
				Gateway string `yaml:"gateway"`
			} `yaml:"config"`
		} `yaml:"ipam"`
	}
	if err := node.Decode(&value); err != nil {
		return fmt.Errorf("compose network YAML: %w", err)
	}
	if len(value.IPAM.Config) > 1 {
		return fmt.Errorf("one IPv4 IPAM subnet is supported")
	}
	if len(value.IPAM.Config) == 1 {
		c := value.IPAM.Config[0]
		if c.Gateway != "" {
			return fmt.Errorf("custom Compose IPAM gateway is unsupported; gateway is the first subnet host")
		}
		value.Subnet = c.Subnet
	}
	*n = Network(value.plain)
	return nil
}
