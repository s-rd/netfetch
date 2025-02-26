package k8s

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Use lipgloss for neat tables in CLI
var HeaderAboveTableStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("10")).
	PaddingLeft(0).
	PaddingRight(0).
	MarginBottom(1)

var FoundPolicyStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("10")).
	Align(lipgloss.Center).
	PaddingLeft(0).
	PaddingRight(4).
	MarginTop(1).
	MarginBottom(1)

var PoliciesNotApplyingHeaderStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("6")).
	Align(lipgloss.Center).
	PaddingLeft(4).
	PaddingRight(4).
	MarginTop(1).
	MarginBottom(1)

var (
	HeaderStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).Align(lipgloss.Center)
	EvenRowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	OddRowStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

func createPodsTable(podsInfo [][]string) string {
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		StyleFunc(func(row, col int) lipgloss.Style {
			switch {
			case row == 0:
				return HeaderStyle
			case row%2 == 0:
				return EvenRowStyle
			default:
				return OddRowStyle
			}
		}).
		Headers("Namespace", "Pod Name", "IP Address")

	for _, row := range podsInfo {
		formattedRow := make([]string, 3)
		for i := 0; i < 3; i++ {
			if i < len(row) {
				formattedRow[i] = row[i]
			} else {
				formattedRow[i] = "N/A"
			}
		}

		t.Row(formattedRow[0], formattedRow[1], formattedRow[2])
	}

	return t.String()
}

func createPoliciesTable(policiesInfo [][]string) string {
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("99"))).
		StyleFunc(func(row, col int) lipgloss.Style {
			switch {
			case row == 0:
				return HeaderStyle
			case row%2 == 0:
				return EvenRowStyle
			default:
				return OddRowStyle
			}
		}).
		Headers("Policy Name")

	for _, row := range policiesInfo {
		t.Row(row...)
	}

	return t.String()
}

// GetCiliumDynamicClient returns a dynamic interface to query for Cilium policies
func GetCiliumDynamicClient() (dynamic.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfigPath := os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			kubeconfigPath = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("cannot create k8s client config: %s", err)
		}
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return dynamicClient, nil
}

var hasStartedCiliumScan bool = false
var globallyProtectedPods = make(map[string]struct{})

// ScanCiliumNetworkPolicies scans namespaces for Cilium network policies
func ScanCiliumNetworkPolicies(specificNamespace string, dryRun bool, returnResult bool, isCLI bool, printScore bool, printMessages bool) (*ScanResult, error) {
	var output bytes.Buffer
	var namespacesToScan []string

	unprotectedPodsCount := 0
	scanResult := new(ScanResult)

	writer := bufio.NewWriter(&output)

	dynamicClient, err := GetCiliumDynamicClient()
	if err != nil {
		fmt.Printf("Error creating dynamic Kubernetes client: %s\n", err)
		return nil, err
	}

	if dynamicClient == nil {
		fmt.Println("Failed to create dynamic client: client is nil")
		return nil, fmt.Errorf("failed to create dynamic client: client is nil")
	}

	clientset, err := GetClientset()
	if err != nil {
		fmt.Printf("Error creating Kubernetes clientset: %s\n", err)
		return nil, err
	}

	if clientset == nil {
		fmt.Println("Failed to create clientset: clientset is nil")
		return nil, fmt.Errorf("failed to create clientset: clientset is nil")
	}

	ciliumNPResource := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumnetworkpolicies",
	}

	// Check if a specific namespace is provided
	if specificNamespace != "" {
		// Verify ns exists
		_, err := clientset.CoreV1().Namespaces().Get(context.TODO(), specificNamespace, metav1.GetOptions{})
		if err != nil {
			if k8serrors.IsNotFound(err) {
				// Namespace does not exist
				return nil, fmt.Errorf("namespace %s does not exist", specificNamespace)
			}
			return nil, fmt.Errorf("error checking namespace %s: %v", specificNamespace, err)
		}
		namespacesToScan = append(namespacesToScan, specificNamespace)
	} else {
		namespaceList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing namespaces: %v", err)
		}
		for _, ns := range namespaceList.Items {
			if !IsSystemNamespace(ns.Name) {
				namespacesToScan = append(namespacesToScan, ns.Name)
			}
		}
	}

	missingPoliciesOrUncoveredPods := false
	userDeniedPolicyApplication := false
	policyChangesMade := false
	deniedNamespaces := []string{}

	if isCLI && !hasStartedCiliumScan {
		fmt.Println("Policy type: Cilium")
		hasStartedCiliumScan = true
	}

	for _, nsName := range namespacesToScan {
		policies, err := dynamicClient.Resource(ciliumNPResource).Namespace(nsName).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			errorMsg := fmt.Sprintf("\nError listing Cilium network policies in namespace %s: %s\n", nsName, err)
			printToBoth(writer, errorMsg)
			return nil, errors.New(errorMsg)
		}

		var unstructuredPolicies []*unstructured.Unstructured
		for i := range policies.Items {
			policy := &policies.Items[i]
			unstructuredPolicies = append(unstructuredPolicies, policy)
		}

		hasDenyAll := HasDefaultDenyAllCiliumPolicy(unstructuredPolicies)
		coveredPods := make(map[string]bool)

		for _, policyUnstructured := range policies.Items {
			if IsDefaultDenyAllCiliumPolicy(policyUnstructured) {
				hasDenyAll = true
			}
			policyMap := policyUnstructured.UnstructuredContent()

			spec, found := policyMap["spec"].(map[string]interface{})
			if !found {
				fmt.Fprintf(writer, "Error finding spec for policy %s in namespace %s\n", policyUnstructured.GetName(), nsName)
				continue
			}

			endpointSelector, found := spec["endpointSelector"].(map[string]interface{})
			if !found {
				fmt.Fprintf(writer, "Error finding endpointSelector for policy %s in namespace %s\n", policyUnstructured.GetName(), nsName)
				continue
			}

			labelSelector, err := ConvertEndpointToSelector(endpointSelector)
			if err != nil {
				fmt.Fprintf(writer, "Error converting endpoint selector to label selector for policy %s: %s\n", policyUnstructured.GetName(), err)
				continue
			}

			pods, err := clientset.CoreV1().Pods(nsName).List(context.TODO(), metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				fmt.Fprintf(writer, "Error listing pods for endpointSelector %s: %s\n", labelSelector, err)
				continue
			}

			for _, pod := range pods.Items {
				coveredPods[pod.Name] = true
			}
		}

		if !hasDenyAll {
			var unprotectedPodDetails [][]string
			allPods, err := clientset.CoreV1().Pods(nsName).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				errorMsg := fmt.Sprintf("Error listing all pods in namespace %s: %s\n", nsName, err)
				printToBoth(writer, errorMsg)
				continue
			}

			for _, pod := range allPods.Items {
				podIdentifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name) // Create a unique identifier for the pod

				// Check if the pod is globally protected. If it is, skip it.
				if _, protectedGlobally := globallyProtectedPods[podIdentifier]; !protectedGlobally {
					// Check if the pod is protected by the policies. If it's protected, it'll also be added to globallyProtectedPods
					if IsPodProtected(writer, clientset, pod, unstructuredPolicies, hasDenyAll, globallyProtectedPods) {
						// [DEBUG] fmt.Printf("Pod %s/%s is now marked as globally protected\n", pod.Namespace, pod.Name)
					} else {
						// Handle unprotected pod
						podDetail := []string{pod.Namespace, pod.Name, pod.Status.PodIP}
						unprotectedPodDetails = append(unprotectedPodDetails, podDetail)
						unprotectedPodsCount++
					}
				}
			}

			if len(unprotectedPodDetails) > 0 {
				missingPoliciesOrUncoveredPods = true

				// Flatten the details and append to scanResult.UnprotectedPods
				for _, podDetail := range unprotectedPodDetails {
					flattenedDetail := strings.Join(podDetail, " ")
					scanResult.UnprotectedPods = append(scanResult.UnprotectedPods, flattenedDetail)
				}
				// If CLI mode, interact with the user
				if isCLI {
					tableOutput := createPodsTable(unprotectedPodDetails)
					headerText := fmt.Sprintf(" Unprotected pods found in namespace %s:", nsName)
					styledHeaderText := HeaderStyle.Render(headerText)
					printToBoth(writer, styledHeaderText+"\n")
					printToBoth(writer, tableOutput+"\n")
					fmt.Printf("\n")

					if !dryRun {
						confirm := false
						prompt := &survey.Confirm{
							Message: fmt.Sprintf("Do you want to add a default deny all Cilium network policy to the namespace %s?", nsName),
						}
						survey.AskOne(prompt, &confirm, nil)
						fmt.Printf("\n")

						if confirm {
							err := CreateAndApplyDefaultDenyCiliumPolicy(nsName, dynamicClient)
							if err != nil {
								fmt.Printf("\nFailed to apply default deny Cilium policy in namespace %s: %s\n", nsName, err)
							} else {
								fmt.Printf("\nApplied default deny Cilium policy in namespace %s\n", nsName)
								policyChangesMade = true
							}
						} else {
							userDeniedPolicyApplication = true
							deniedNamespaces = append(deniedNamespaces, nsName)
						}
					}
				} else {
					// Non-CLI behavior
					scanResult.DeniedNamespaces = append(scanResult.DeniedNamespaces, nsName)
				}
			}
		}
	}

	writer.Flush()
	if output.Len() > 0 {
		saveToFile := false
		prompt := &survey.Confirm{
			Message: "Do you want to save the output to netfetch-cilium.txt?",
		}
		survey.AskOne(prompt, &saveToFile, nil)

		if saveToFile {
			err := os.WriteFile("netfetch-cilium.txt", output.Bytes(), 0644)
			if err != nil {
				errorFileMsg := fmt.Sprintf("Error writing to file: %s\n", err)
				printToBoth(writer, errorFileMsg)
			} else {
				printToBoth(writer, "Output file created: netfetch-cilium.txt\n")
			}
		} else {
			printToBoth(writer, "Output file not created.\n")
		}
	}

	score := CalculateScore(!missingPoliciesOrUncoveredPods, !userDeniedPolicyApplication, unprotectedPodsCount)
	scanResult.Score = score

	if printMessages {
		if policyChangesMade {
			fmt.Println("\nChanges were made during this scan. It's recommended to re-run the scan for an updated score.")
		}

		if missingPoliciesOrUncoveredPods {
			if userDeniedPolicyApplication {
				printToBoth(writer, "\nFor the following namespaces, you should assess the need of implementing network policies:\n")
				for _, ns := range deniedNamespaces {
					fmt.Println(" -", ns)
				}
				printToBoth(writer, "\nConsider either an implicit default deny all network policy or a policy that targets the pods not selected by a cilium network policy. Check the Cilium documentation for more information on cilium network policies: https://docs.cilium.io/en/latest/security/policy/\n")
			} else {
				printToBoth(writer, "\nNetfetch scan completed!\n")
			}
		} else {
			printToBoth(writer, "\nNo cilium network policies missing. You are good to go!\n")
		}
	}

	if printScore {
		// Print the final score
		fmt.Printf("\nYour Netfetch security score is: %d/42\n", score)
	}

	hasStartedCiliumScan = false
	return scanResult, nil
}

// ScanCiliumClusterwideNetworkPolicies scans the cluster for Cilium Clusterwide Network Policies
func ScanCiliumClusterwideNetworkPolicies(dynamicClient dynamic.Interface, printMessages bool, dryRun bool, isCLI bool) (*ScanResult, error) {
	// Buffer and writer setup to capture output for both console and file.
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)

	// Check for a valid dynamic client
	if dynamicClient == nil {
		fmt.Println("Failed to create dynamic client: client is nil")
		return nil, fmt.Errorf("failed to create dynamic client: client is nil")
	}

	// Attempt to create a Kubernetes clientset
	clientset, err := GetClientset()
	if err != nil {
		fmt.Printf("Error creating Kubernetes clientset: %s\n", err)
		return nil, err
	}

	if clientset == nil {
		fmt.Println("Failed to create clientset: clientset is nil")
		return nil, fmt.Errorf("failed to create clientset: clientset is nil")
	}

	// Define the resource for Cilium Clusterwide Network Policies
	ciliumCCNPResource := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumclusterwidenetworkpolicies",
	}

	// Fetch the policies from the cluster
	policies, err := dynamicClient.Resource(ciliumCCNPResource).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		printToBoth(writer, fmt.Sprintf("Error listing CiliumClusterwideNetworkPolicies: %s\n", err))
		return nil, fmt.Errorf("error listing CiliumClusterwideNetworkPolicies: %v", err)
	}

	// Deduplicate policies by storing them in a map to check for uniqueness
	policyMap := make(map[string]bool)
	var unstructuredPolicies []*unstructured.Unstructured

	for i := range policies.Items {
		policy := policies.Items[i]
		policyName := policy.GetName()

		// Check if the policy has already been added to the map (and thus the list)
		if _, exists := policyMap[policyName]; !exists {
			// If it doesn't exist, add it to the map and the list
			policyMap[policyName] = true
			unstructuredPolicies = append(unstructuredPolicies, &policies.Items[i]) // Reference directly from the original slice
		}
	}

	if isCLI && !hasStartedCiliumScan {
		hasStartedCiliumScan = true
	}

	// Report the detected policies
	if isCLI {
		if len(policies.Items) == 0 {
			printToBoth(writer, "No policies found.\n")
		} else {
			printToBoth(writer, "[VERBOSE]: Found:\n")
			for _, policy := range policies.Items {
				policyName, _, _ := unstructured.NestedString(policy.UnstructuredContent(), "metadata", "name")
				printToBoth(writer, "- "+policyName+"\n")
			}
		}
	}

	// Initialize the scan result
	scanResult := &ScanResult{
		NamespacesScanned:  []string{"cluster-wide"},
		DeniedNamespaces:   []string{},
		UnprotectedPods:    []string{},
		PolicyChangesMade:  false,
		UserDeniedPolicies: false,
		AllPodsProtected:   false,
		HasDenyAll:         []string{},
		Score:              0, // or some initial value
	}

	// Initialize variables to track policies
	var defaultDenyAllFound, appliesToEntireCluster, partialDenyAllFound bool
	var partialDenyAllPolicies []string // To hold names of policies that don't apply to the entire cluster

	// Iterate through each policy to determine its type
	for _, policy := range policies.Items {
		isDenyAll, isClusterWide := IsDefaultDenyAllCiliumClusterwidePolicy(policy)
		if isDenyAll {
			defaultDenyAllFound = true
			if isClusterWide {
				appliesToEntireCluster = true
				scanResult.AllPodsProtected = true
			} else {
				// Track policies that are default deny but don't apply to the entire cluster
				partialDenyAllFound = true
				policyName, _, _ := unstructured.NestedString(policy.UnstructuredContent(), "metadata", "name")
				partialDenyAllPolicies = append(partialDenyAllPolicies, policyName)
			}
		}
	}

	// Report findings based on the policy types found
	if appliesToEntireCluster {
		printToBoth(writer, "Cluster wide default deny all policy detected.\n")
	} else {
		var promptForPolicyCreation bool

		var policiesForTable [][]string

		if !appliesToEntireCluster && partialDenyAllFound && defaultDenyAllFound {
			for _, pName := range partialDenyAllPolicies {
				// Append policy names to the slice for the table
				policiesForTable = append(policiesForTable, []string{pName})
			}

			// Generate the table output for partial policies
			tableOutput := createPoliciesTable(policiesForTable)

			// Render the headers with styles
			partialPoliciesHeader := HeaderStyle.Render("Cluster wide policies in effect:")

			// Print the headers and the table output
			printToBoth(writer, partialPoliciesHeader+"\n")
			printToBoth(writer, tableOutput+"\n")
			promptForPolicyCreation = true
		} else if !defaultDenyAllFound {
			promptForPolicyCreation = true
		}

		if promptForPolicyCreation && isCLI && !dryRun {
			// Prompt to create a default deny-all policy
			createPolicy := false
			prompt := &survey.Confirm{
				Message: "Do you want to create a cluster wide default deny all cilium network policy?",
			}
			survey.AskOne(prompt, &createPolicy, nil)
			fmt.Printf("\n")

			if createPolicy && !dryRun {
				err := CreateAndApplyDefaultDenyCiliumClusterwidePolicy(dynamicClient)
				if err != nil {
					printToBoth(writer, fmt.Sprintf("\nFailed to apply default deny Cilium clusterwide policy: %s\n", err))
				} else {
					printToBoth(writer, "\nApplied cluster wide default deny cilium policy\n")
					scanResult.PolicyChangesMade = true
				}
			} else {
				scanResult.UserDeniedPolicies = true
			}
		}
	}

	pods, err := clientset.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		printToBoth(writer, fmt.Sprintf("Error listing pods: %v\n", err))
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	defaultDenyAllExists := appliesToEntireCluster

	// Check each pod to see if it's protected by the policies
	for _, pod := range pods.Items {
		if IsPodProtected(writer, clientset, pod, unstructuredPolicies, defaultDenyAllExists, globallyProtectedPods) {
			podIdentifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			globallyProtectedPods[podIdentifier] = struct{}{}
		} else {
			unprotectedPods := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			scanResult.UnprotectedPods = append(scanResult.UnprotectedPods, unprotectedPods)
		}
	}

	if len(scanResult.UnprotectedPods) > 0 {
		printToBoth(writer, fmt.Sprintf("Found %d pods not targeted by a cluster wide policy. The namespaced scan will be initiated..\n", len(scanResult.UnprotectedPods)))
	} else {
		printToBoth(writer, "All pods are protected by cluster wide policies.\n")
	}

	if printMessages {
		printToBoth(writer, "\nCluster wide cilium network policy scan completed!\n")
	}

	writer.Flush()
	if output.Len() > 0 {
		saveToFile := false
		prompt := &survey.Confirm{
			Message: "Do you want to save the output to netfetch-clusterwide-cilium.txt?",
		}
		survey.AskOne(prompt, &saveToFile, nil)

		if saveToFile {
			err := os.WriteFile("netfetch-clusterwide-cilium.txt", output.Bytes(), 0644)
			if err != nil {
				printToBoth(writer, fmt.Sprintf("Error writing to file: %s\n", err))
			} else {
				printToBoth(writer, "Output file created: netfetch-clusterwide-cilium.txt\n")
			}
		} else {
			printToBoth(writer, "Output file not created.\n")
		}
	}

	hasStartedCiliumScan = true
	return scanResult, nil
}

func IsPodProtected(writer *bufio.Writer, clientset *kubernetes.Clientset, pod corev1.Pod, policies []*unstructured.Unstructured, defaultDenyAllExists bool, globallyProtectedPods map[string]struct{}) bool {
	podIdentifier := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if _, protected := globallyProtectedPods[podIdentifier]; protected {
		printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is already globally covered\n", podIdentifier))
		return true
	}

	if defaultDenyAllExists {
		printToBoth(writer, fmt.Sprintf("[VERBOSE]: Default deny-all policy exists, marking pod %s as protected\n", podIdentifier))
		globallyProtectedPods[podIdentifier] = struct{}{}
		return true
	}

	// Loop through policies to find any that apply namespace-wide.
	for _, policy := range policies {
		policyName := policy.GetName()

		spec, found := policy.UnstructuredContent()["spec"].(map[string]interface{})
		if !found {
			printToBoth(writer, fmt.Sprintf("No spec found in policy %s\n", policyName))
			continue
		}

		endpointSelector, found, err := unstructured.NestedMap(spec, "endpointSelector", "matchLabels")
		if err != nil {
			printToBoth(writer, fmt.Sprintf("Error reading endpointSelector from policy %s: %v\n", policy.GetName(), err))
			continue
		}
		if !found {
			printToBoth(writer, fmt.Sprintf("No endpointSelector found in policy %s\n", policyName))
			continue
		}

		// Check if the policy applies to the entire namespace.
		if val, ok := endpointSelector["io.kubernetes.pod.namespace"]; ok && val == pod.Namespace {
			printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is covered by cluster wide policy %s\n", podIdentifier, policyName))
			globallyProtectedPods[podIdentifier] = struct{}{}
			return true
		}
	}

	for _, policy := range policies {
		policyName := policy.GetName()

		spec, found := policy.UnstructuredContent()["spec"].(map[string]interface{})
		if !found {
			printToBoth(writer, fmt.Sprintf("No spec found in policy %s\n", policyName))
			continue
		}

		endpointSelector, _, _ := unstructured.NestedMap(spec, "endpointSelector", "matchLabels")
		isDenyAll, appliesToEntireCluster := IsDefaultDenyAllCiliumClusterwidePolicy(*policy)

		if isDenyAll && appliesToEntireCluster {
			printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is covered by deny-all policy %s\n", podIdentifier, policyName))
			globallyProtectedPods[podIdentifier] = struct{}{}
			return true
		}

		if MatchesLabels(pod.Labels, endpointSelector) {
			ingress, foundIngress, _ := unstructured.NestedSlice(spec, "ingress")
			egress, foundEgress, _ := unstructured.NestedSlice(spec, "egress")

			// Existing checks for empty ingress/egress and deny-all
			if (foundIngress && (IsEmptyOrOnlyContainsEmptyObjects(ingress) || IsSpecificallyEmpty(ingress))) || (foundEgress && (IsEmptyOrOnlyContainsEmptyObjects(egress) || IsSpecificallyEmpty(egress))) || isDenyAll {
				printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is covered by deny-all policy %s\n", podIdentifier, policyName))
				globallyProtectedPods[podIdentifier] = struct{}{}
				return true
			}

			// New check for specific ingress or egress rules
			if foundIngress && !IsEmptyOrOnlyContainsEmptyObjects(ingress) || foundEgress && !IsEmptyOrOnlyContainsEmptyObjects(egress) {
				printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is covered by policy %s with specific rules\n", podIdentifier, policyName))
				globallyProtectedPods[podIdentifier] = struct{}{}
				return true
			}
		}
	}

	printToBoth(writer, fmt.Sprintf("[VERBOSE]: Pod %s is not covered by any policy\n", podIdentifier))
	return false
}

// Check specifically for a slice that only contains a single empty map ({}), representing a default deny.
func IsSpecificallyEmpty(slice []interface{}) bool {
	return len(slice) == 1 && len(slice[0].(map[string]interface{})) == 0
}

// // Placeholder function for future reference
// func entityMatchesPod(entity string, pod corev1.Pod) bool {
// 	switch entity {
// 	case "all":
// 		// All always matches any pod
// 		return true
// 	case "world":
// 		// Determine if the pod communicates with entities outside the cluster
// 		// This might involve checking the pod's networking configuration, labels, or annotations
// 		// Placeholder logic: return false for now
// 		return false
// 	case "host":
// 		// Check if the pod is using host networking
// 		if pod.Spec.HostNetwork {
// 			return true
// 		}
// 		return false
// 	case "remote-node":
// 		// Check if the pod is intended to communicate with a remote node
// 		// This might involve checking node labels, pod's node affinity, or annotations
// 		// Placeholder logic: return false for now
// 		return false
// 	default:
// 		// Unknown entity type, log it, handle as needed
// 		fmt.Printf("Unknown entity type encountered: %s\n", entity)
// 		return false
// 	}
// }

// MatchesLabels checks if the pod's labels match the policy's endpointSelector
func MatchesLabels(podLabels map[string]string, policySelector map[string]interface{}) bool {
	for key, value := range policySelector {
		if val, ok := value.(string); ok {
			if podVal, podOk := podLabels[key]; !podOk || podVal != val {
				return false
			}
		} else {
			fmt.Printf("Policy label value %v is not a string\n", value)
			return false
		}
	}
	return true
}

// ConvertEndpointToSelector converts the endpointSelector from a CiliumNetworkPolicy to a label selector string.
func ConvertEndpointToSelector(endpointSelector map[string]interface{}) (string, error) {
	matchLabels, found := endpointSelector["matchLabels"].(map[string]interface{})
	if !found || len(matchLabels) == 0 {
		return "", nil
	}

	var selectorParts []string
	for key, val := range matchLabels {
		if strVal, ok := val.(string); ok {
			selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", key, strVal))
		} else {
			return "", fmt.Errorf("value for %s in matchLabels is not a string", key)
		}
	}

	labelSelector := strings.Join(selectorParts, ",")
	return labelSelector, nil
}

// CreateAndApplyDefaultDenyCiliumClusterwidePolicy creates and applies a default deny all network policy for Cilium at the cluster level.
func CreateAndApplyDefaultDenyCiliumClusterwidePolicy(dynamicClient dynamic.Interface) error {
	// Construct the policy
	denyAllPolicy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumClusterwideNetworkPolicy",
			"metadata": map[string]interface{}{
				"name": "clusterwide-default-deny-all",
			},
			"spec": map[string]interface{}{
				"endpointSelector": map[string]interface{}{
					"matchLabels": map[string]string{},
				},
				"ingress": []interface{}{},
				"egress":  []interface{}{},
			},
		},
	}

	// Set the GVR
	ciliumCCNPResource := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumclusterwidenetworkpolicies",
	}

	// Create the policy
	_, err := dynamicClient.Resource(ciliumCCNPResource).Create(context.Background(), denyAllPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create default deny all CiliumClusterwideNetworkPolicy: %v", err)
	}

	return nil
}

// IsDefaultDenyAllCiliumClusterwidePolicy checks if a single CiliumClusterwideNetworkPolicy is a default deny-all policy
func IsDefaultDenyAllCiliumClusterwidePolicy(policyUnstructured unstructured.Unstructured) (bool, bool) {
	spec, found := policyUnstructured.UnstructuredContent()["spec"].(map[string]interface{})
	if !found {
		fmt.Println("Spec not found in policy.")
		return false, false
	}

	ingress, ingressFound := spec["ingress"].([]interface{})
	egress, egressFound := spec["egress"].([]interface{})

	// Check if ingress and egress are nil or only contain empty objects
	isIngressEmpty := !ingressFound || IsEmptyOrOnlyContainsEmptyObjects(ingress)
	isEgressEmpty := !egressFound || IsEmptyOrOnlyContainsEmptyObjects(egress)

	// Check for default deny-all
	denyAll := isIngressEmpty && isEgressEmpty

	// Check if it applies to the entire cluster
	endpointSelector, selectorFound := spec["endpointSelector"].(map[string]interface{})
	appliesToEntireCluster := !selectorFound || (len(endpointSelector) == 0 || isEndpointSelectorEmpty(endpointSelector))

	return denyAll, appliesToEntireCluster
}

// Helper function to check if the ingress/egress slice is empty or only contains empty objects
func IsEmptyOrOnlyContainsEmptyObjects(slice []interface{}) bool {
	if len(slice) == 0 {
		return true
	}
	for _, item := range slice {
		if itemMap, ok := item.(map[string]interface{}); !ok || len(itemMap) != 0 {
			return false
		}
	}
	return true
}

// Helper function to check if the endpointSelector is effectively empty
func isEndpointSelectorEmpty(selector map[string]interface{}) bool {
	matchLabels, found := selector["matchLabels"].(map[string]interface{})
	return !found || len(matchLabels) == 0
}

// CreateAndApplyDefaultDenyCiliumPolicy creates and applies a default deny all network policy for Cilium in the specified namespace.
func CreateAndApplyDefaultDenyCiliumPolicy(namespace string, dynamicClient dynamic.Interface) error {
	// Construct the policy name dynamically
	policyName := namespace + "-cilium-default-deny-all"

	// Define the policy
	denyAllPolicy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2",
			"kind":       "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name":      policyName,
				"namespace": namespace,
			},
			"spec": map[string]interface{}{
				"endpointSelector": map[string]interface{}{
					"matchLabels": map[string]string{},
				},
				"ingress": []interface{}{},
				"egress":  []interface{}{},
			},
		},
	}

	// Set the GVR
	ciliumNPResource := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumnetworkpolicies",
	}

	// Create the policy
	_, err := dynamicClient.Resource(ciliumNPResource).Namespace(namespace).Create(context.TODO(), denyAllPolicy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create default deny all CiliumNetworkPolicy: %v", err)
	}

	fmt.Printf("Applied default deny all CiliumNetworkPolicy to namespace %s\n", namespace)
	return nil
}

// HasDefaultDenyAllCiliumPolicy checks if the list of CiliumNetworkPolicies includes a default deny all policy
func HasDefaultDenyAllCiliumPolicy(policies []*unstructured.Unstructured) bool {
	for _, policy := range policies {
		if IsDefaultDenyAllCiliumPolicy(*policy) {
			return true
		}
	}
	return false
}

// IsDefaultDenyAllCiliumPolicy checks if a single Cilium policy is a default deny-all policy
func IsDefaultDenyAllCiliumPolicy(policyUnstructured unstructured.Unstructured) bool {
	spec, found := policyUnstructured.UnstructuredContent()["spec"].(map[string]interface{})
	if !found {
		return false
	}

	ingress, ingressFound := spec["ingress"]
	egress, egressFound := spec["egress"]
	return (!ingressFound || len(ingress.([]interface{})) == 0) && (!egressFound || len(egress.([]interface{})) == 0)
}
